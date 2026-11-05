## One step, one container

[[/images/fresh-runtime.svg|alt=Attempt 1 runs in container A and fails; Temporal retries in a fresh container B that can't see A's files; only B's output reaches Temporal, loom's Record and the Git store, which is outside every container]]

`Runner.Run` starts a container, writes the input to its standard input, waits, and returns its standard output. The command line is fixed; `Spec.Argv` returns it, and `TestInv_ExactPodmanCommandLine` checks it word for word:

```text
podman run --rm -i --name bobbin-run-<random> --pull=never --network=none --cap-drop=all
           --security-opt=no-new-privileges --pids-limit=256 --memory=512m
           --log-driver=none --http-proxy=false [--timeout=N]
           --label heedle.kind=KIND [--label heedle.NAME=VALUE ...] [--env KEY=VALUE ...]
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
| `--label heedle.NAME=VALUE` | Marks the container as heedle's: its kind, and the run and step it belongs to | So heedle can list, stop and clean up its own containers, and never anyone else's ([below](#heedles-labels-and-tidying-up)) |
| `--env KEY=VALUE` | Sets a variable inside the container | Only the variables in `Spec.Env`; nothing is inherited from the host |
| `--env NAME` | Sets a secret inside the container, its value taken from podman's own environment | Only the names in `Spec.Secrets`, whose values bobbin puts in podman's environment, so they never appear on a command line any process on the host can read |

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
| `Command(command)` | `podman exec --interactive --workdir /work … /bin/sh -c command` | A long-running command whose standard input and output the caller streams, such as an MCP server speaking over them; it ends when its context does, or with the session |
| `ReadTo(path, w, max)` | `podman exec … cat` | Streams a file of any size out to a writer, up to `max` bytes, so a large file never has to fit in memory |
| `Diff()` | `podman diff --format json` | Lists every path the session added, changed or deleted, relative to the image |
| `Stop()` | `podman exec … kill -9 -1` | Kills commands left running, keeping the files |
| `Close()` | `podman rm --force --time 0 --ignore` | Removes the container and everything in it |

The image needs `/bin/sh`, and either a `/work` the container's user can write or permission to create it. With every capability dropped, root inside a bare `FROM scratch` image can't create directories at the top level, so such images should include `/work`. `Stop()` kills every process the session's commands left running (`kill -9 -1` inside it): a command cut off by its context otherwise keeps running. A session lasts at most 30 minutes (`MaxSessionLife`, or its context's deadline): Podman stops it then (`--timeout`) and `--rm` removes it, so even a session whose worker died never outlives it. One session is one step's workspace: it's never reused for another step or run.

## Image builds

The `image` command builds an image with `podman build`, from a Containerfile written in the program:

```lisp
(pipe (agent "Write app.json for the service.")
      (image "FROM docker.io/library/alpine@sha256:<digest>
COPY in/n0.txt /app/agent-result.json"))
```

Its build context is a fresh temporary directory holding only the Containerfile and `in/<node>.txt` for each input, so `COPY` sees what earlier steps produced and nothing else from the worker. The build runs exactly as `BuildArgv` says:

| Flag | Why |
|---|---|
| `--pull=never` | Only images already on the worker: every base must be pinned to content, so the same Containerfile always starts from the same bytes |
| `--network=none` | `RUN` steps have no network |
| `--security-opt=no-new-privileges` | setuid binaries gain nothing |
| `--memory=2g` | Bounded memory (more than a step's container: compilers run here) |
| `--ulimit cpu=1800` | Each `RUN` process gets 30 minutes of CPU time, so a busy loop ends |
| `--http-proxy=false` | The worker's proxy variables stay out |
| `--timestamp=0`, `--identity-label=false` | Timestamps and the builder's version don't change the image ID |

`RUN` steps keep Podman's default, reduced capabilities rather than none: package managers need to own and chmod files, and dropping them all would break ordinary builds.

Before building, `CheckContainerfile` reads the Containerfile with the parser Podman's builder itself uses ([openshift/imagebuilder](https://github.com/openshift/imagebuilder), at the version buildah 1.33 in Podman 4.9 uses), so it sees the instructions and flags the build will act on, however the file is laid out (comments and blank lines inside a continued line included). It refuses:

- a `FROM` or `COPY --from` that isn't `scratch`, an earlier stage, or an image pinned to content (`name@sha256:…` or a full image ID), or that holds a variable (`$`); stage names must be plain (`[a-z][a-z0-9_.-]*`);
- flags on `FROM` or `RUN` (such as `RUN --network` or `--mount`), and `COPY` flags beyond `--from`, `--chown`, `--chmod` and `--link`;
- any instruction but `FROM`, `RUN`, `COPY`, `WORKDIR`, `ENV`, `LABEL`, `CMD`, `ENTRYPOINT`, `USER`, `EXPOSE`, `ARG`, `STOPSIGNAL`, `HEALTHCHECK`, `SHELL`, `VOLUME` and `MAINTAINER`: so no `ADD` (it fetches URLs and unpacks archives: use `COPY`) and no `ONBUILD`;
- heredocs, and an escape character other than `\`.

Then `podman image exists` checks that every pinned base is on the worker, since builds never pull.

The output is JSON, `{"image": "sha256:<hex>"}`: an image ID that a later program can run with `(container "<that ID>" …)`. Built images stay in the worker's Podman storage; `podman image prune` removes unused ones.

What a build doesn't bound, plainly:

- **A cancelled build keeps running.** Podman runs each `RUN` step in its own session, so stopping `podman build` (a step's timeout, a cancelled run) leaves the step running until it ends; the CPU limit ends a busy loop, but not one that waits. Every retry starts a new build.
- **Processes and disk.** `RUN` steps have no process limit, and what they write goes to the worker's Podman storage.
- **A base image's `ONBUILD` triggers** run, inside the same sandbox, like the Containerfile's own `RUN` steps. Bases are only images already on the worker.
- **Same image ID.** A build whose `RUN` steps are deterministic gives the same ID anywhere; one that isn't (reading `/dev/urandom`, say) gives the same ID again only on the same worker, from Podman's layer cache.

### Publishing

`(image "<Containerfile>" "push")` also pushes the image, and only then: to the worker's `-image-registry` repository (such as `ghcr.io/acme/heedle-images`), tagged `sha256-<hex>`, with the worker's own `podman login`. The program never sees the credentials, and without a registry the step fails at once with `ImageSetup`. The output gains `"pushed": "<repo>@sha256:<manifest digest>"`.

A build or push that fails is retryable (a push the registry refuses for lack of a login too, so give the worker its `podman login` first); a Containerfile the check refuses fails with `InvalidInput`, and a base image the worker doesn't have with `ContainerSetup`. The step's plan node carries a 30-minute timeout (`ImageOptions`).

## heedle's labels, and tidying up

Every container bobbin starts carries `heedle.kind`; one started for a step of a run also carries the run and the step (roadmap 9.2). These labels are how heedle finds its own containers, and the only way it tells them apart from anything else on the machine.

| Label | Value |
|---|---|
| `heedle.kind` | `agent`, `skill`, `container`, `connector`, `browse` or `act`. With none given, a single run is a `container` and a session an `agent` |
| `heedle.workflow` | The Temporal workflow that scheduled the step, as Temporal reports it: for a loom run, its plan workflow, `<run's workflow ID>/plan` |
| `heedle.run` | That workflow's run ID |
| `heedle.step` | The step's node ID in the plan |

`Spec.Labels` takes them. What a step's container doesn't name itself comes from the context: `StepLabels(ctx, node)` reads the workflow and its run from Temporal's activity info, and `WithLabels` lets everything started under a step's context carry them. loom sets them where every step's activity starts, and the `container` command labels its own containers. The `container` and `image` steps heartbeat to Temporal while they run, so cancelling a run reaches them. A value is printable ASCII without spaces, quotes, commas or `=`, starting with a letter or digit, at most 255 bytes. `Run` and `Start` check the labels a context carries as they check a Spec's own: `Spec.Validate` refuses anything else as `ErrInvalid` before Podman runs, and `StepLabels` leaves such a value out, so labelling never fails a step. Each label is one `--label NAME=VALUE` argument after the fixed flags, so no value can become a flag. Sessions keep their old `bobbin.session=1` label too.

A `container` step heartbeats to Temporal while its container runs. Temporal tells an activity it was cancelled only when the activity heartbeats, so cancelling a run (`loom cancel`) ends the step. Each run is named (`--name bobbin-run-<random>`, `Spec.NamedArgv`). When a step is stopped (its run cancelled, a deadline) or writes too much, podman is told to stop, and `Run` then removes the container by that name, as a session's `Close` does. So a container whose first process ignores SIGTERM, such as `sleep`, can't outlive its step.

Five calls see and tidy what heedle left on the machine, each one Podman command:

| Call | What it does | Podman |
|---|---|---|
| `Runner.Containers` | Lists heedle's containers: those labelled `heedle.kind` with one of the six kinds, with their labels, image, age, state, and the CPU and memory of running ones | `podman ps --all --filter label=heedle.kind --format json`, then `podman stats --no-stream --format json ID...` |
| `Runner.Remove(which)` | Lists heedle's containers again and removes those `which` picks. It can only remove what that listing found | `podman rm --force --time 0 --ignore ID...` |
| `Runner.Images(repo, keep)` | Lists the images named under `repo` (heedle's is `HeedleRepo`, `ghcr.io/heedle-ai/`), each once, marking those whose name, digest or ID is in `keep` | `podman images --format json` |
| `Runner.PruneImages(repo, keep)` | For each image named under `repo` that isn't kept and that no container uses, removes its names under `repo`, one at a time and never forced. An image that also has a name of your own keeps that name, and Podman keeps the image; one left with no name is deleted | `podman image rm NAME` |
| `Runner.Machine` | Podman's virtual machine on macOS: CPUs, memory and disk (nil on Linux, which needs none) | `podman machine list --format json` |

`repo` must be a registry and an organization ending in `/`, so a prune can never mean "every image". It also needs at least one image to keep, so it can't remove the release's own. To see heedle's containers with Podman itself:

```bash
podman ps --all --filter label=heedle.kind
```

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
