## One step, one container

[[/images/fresh-runtime.svg|alt=Attempt 1 runs in container A and fails; Temporal retries in a fresh container B that can't see A's files; only B's output reaches Temporal, loom's Record and the Git store, which is outside every container]]

`Runner.Run` starts a container, writes the input to its standard input, waits, and returns its standard output. The command line is fixed; `Spec.Argv` returns it, and `TestInv_ExactPodmanCommandLine` checks it word for word:

```text
podman run --rm -i --pull=never --network=none --cap-drop=all
           --security-opt=no-new-privileges --pids-limit=256 --memory=512m
           --log-driver=none --http-proxy=false [--timeout=N] [--env KEY=VALUE ...]
           IMAGE [ARGS ...]
```

Every flag comes before the image. Podman stops reading its own flags at the image, so arguments such as `--volume=/:/host` after it go to the agent as plain arguments, never to Podman.

| Flag | What it does | Why |
|---|---|---|
| `run --rm` | New container, removed when it exits | Its writable layer, and everything the agent wrote, is deleted. Nothing carries over to the next step, run or retry |
| `-i` | Keeps standard input open | The upstream outputs arrive on stdin |
| `--pull=never` | Only images already on this machine | No download in the middle of a run; an image that isn't present fails at once with `ErrSetup` |
| `--network=none` | Only a loopback interface | Agents can't reach the network, including other services on the host |
| `--cap-drop=all` | No Linux capabilities | Root inside the container can't change network, mount or kernel settings |
| `--security-opt=no-new-privileges` | `setuid` binaries can't gain privileges | Closes the usual escalation path inside an image |
| `--pids-limit=256`, `--memory=512m` | Process and memory limits | A runaway agent can't exhaust the machine |
| `--log-driver=none` | Podman keeps no copy of the output | Output goes only to bobbin |
| `--http-proxy=false` | Podman doesn't copy the host's proxy variables in | By default it does, so a proxy URL with credentials would reach every agent (found in review 4) |
| `--timeout=N` | Podman stops the container after N seconds | Set from the context deadline, such as the activity's start-to-close timeout |
| `--env KEY=VALUE` | Sets a variable inside the container | Only the variables in `Spec.Env`; nothing is inherited from the host |

There are no `--volume`, `--mount`, `--tmpfs`, `--device`, `--publish` or host-namespace flags, and `Spec` has no field that could carry one: a container sees only its image, arguments, environment and stdin. `TestInv_NoMounts` fails if any other flag-like string appears in the package, or a field is added to `Spec`.

The `podman` process itself runs with a cleaned environment: variables that could point it at another machine, storage or configuration (`CONTAINER*`, `PODMAN*`) are dropped, and it runs in the C locale.

## Where the overlay filesystem comes in

A container's filesystem is the image's read-only layers plus one empty writable layer on top, joined by the kernel's overlay filesystem. Everything the agent writes, installs or deletes lands in that top layer. `--rm` deletes it with the container. The next call starts from the same read-only image with a new empty layer, so each step sees exactly the image, every time.

The image is referenced by content: a full 64-hex image ID (with or without `sha256:`) or `name@sha256:<digest>`. Tags (`alpine:3.20`), short IDs and anything else are refused with `ErrInvalid` before Podman runs, because what they name can change.

## Sessions: one workspace for one step's commands

An agent step runs many commands, each building on the last: write a file, run a script on it, read the result. For that, `Runner.Start` starts a **session**: one fresh container with exactly the same hardening as a single run (no network, no Linux capabilities, process and memory limits, nothing mounted, nothing inherited from the machine), named `bobbin-session-<random>` and labelled `bobbin.session=1`.

| Call | Podman | What it does |
|---|---|---|
| `Start(spec)` | `podman run --detach --rm …`, then `podman exec … /bin/mkdir -p /work` | Starts the container with `/bin/sh` waiting, and creates `/work` |
| `Exec(command, stdin)` | `podman exec --interactive --workdir /work … /bin/sh -c command` | Runs one command in `/work`; its exit code, and up to 64 KiB each of standard output and error, are the result |
| `Write(path, data)` / `Read(path, max)` | `podman exec … cat` | Puts a file in, or reads one out; the path is an argument, never part of a command |
| `Diff()` | `podman diff --format json` | Lists every path the session added, changed or deleted, relative to the image |
| `Stop()` | `podman exec … kill -9 -1` | Kills commands left running, keeping the files |
| `Close()` | `podman rm --force --time 0 --ignore` | Removes the container and everything in it |

The image needs `/bin/sh`, and either a `/work` the container's user can write or permission to create it. With every capability dropped, root inside a bare `FROM scratch` image can't create directories at the top level, so such images should include `/work`. `Stop()` kills every process the session's commands left running (`kill -9 -1` inside it): a command cut off by its context otherwise keeps running. A session lasts at most 30 minutes (`MaxSessionLife`, or its context's deadline): Podman stops it then (`--timeout`) and `--rm` removes it, so even a session whose worker died never outlives it. One session is one step's workspace: it's never reused for another step or run.

## Retries get a fresh container

Inside a Temporal workflow, a step that fails is retried by Temporal's retry policy. With bobbin, the retry is a new `podman run`: a fresh container from the same image. A failed attempt can't leave files, processes or a half-written state for the retry to trip over. `TestContainer_RetryRunsInAFreshContainer` checks it: attempt 1 writes `/scratch` and exits 3; attempt 2 doesn't see `/scratch`.

## Errors

| Situation | `Run` returns | In a program (Temporal error type) | Retried? |
|---|---|---|---|
| Image not pinned, argument or variable with NUL or invalid UTF-8, bad variable name | `ErrInvalid` | `InvalidInput` | No |
| `podman` not found; Podman exits 125 (its own error, such as image not known), 126 or 127 (command can't run), ending stderr with its `Error: ` line | `ErrSetup` | `ContainerSetup` | No |
| More than `MaxOutput` (1 MiB) on stdout; the container is stopped | `ErrOutputTooLarge` | `OutputTooLarge` | No |
| The agent exits with any other non-zero code, including its own 125–127 | `*ExitError` with the code and the last 2 KiB of stderr | retryable | Yes, in a fresh container |
| The context is cancelled | the context's error | | |

An agent can exit 125–127 itself; that's still its own, retryable failure. Only an exit that ends with Podman's own `Error: ` line counts as setup. An agent whose last stderr line starts with `Error: ` and that exits 125–127 is taken for a setup failure and not retried, so avoid that combination.

## Outputs stay small

An output becomes an activity result in Temporal's history, and then a file in the warp store. Both are meant for small values, so stdout is capped at 1 MiB. A step that produces something large should store it elsewhere and output a reference.

## What isn't isolated

- **The kernel.** On Linux, containers share the host's kernel; namespaces and cgroups separate them, but this isn't a virtual machine. On macOS, everything runs inside Podman's Linux VM, which is separate from macOS itself.
- **CPU time and disk space.** There's no CPU quota, and the writable layer's size isn't limited. The context deadline bounds the run time. This matters more for sessions, whose commands an agent chooses for up to 30 minutes: a command can fill the disk Podman's storage is on.
- **Rootful Podman.** Run workers with rootless Podman. Under rootful Podman, root in a container is root on the host minus its capabilities; rootless maps it to your own user.
- **The host's Podman configuration.** `containers.conf` on the host can add volumes, mounts or environment variables to every container, and Podman applies them to bobbin's too. bobbin can't switch those off from the command line (pointing Podman at an empty configuration would also drop the machine connection on macOS). Keep the configuration of the account that runs workers free of them.
- **The image's contents.** bobbin runs exactly the image you pinned; it doesn't scan or verify it. Build images you trust (`FROM scratch` with a static binary is the smallest).

**Planned:** a network allowlist for agents that must call specific services; CPU and disk quotas; signature verification for images from registries; a stronger sandbox (gVisor or Kata) as an option, through Podman's `--runtime`.
