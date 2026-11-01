**bobbin** gives every agent step a runtime of its own. Each step runs in a brand-new [Podman](https://podman.io) container: a pinned image, an empty writable layer, no network, nothing from the host. When the step ends, the container and everything it wrote are gone; only its standard output comes back.

```lisp
(pipe (par (echo "hello") (echo "world"))
      (container "5c5f488453940eab6dedebec814e34eb36317689cae82226a8bc606bb56d0642"))
```

That program runs the [example agent](Writing-Agents) on the two upstream outputs and returns `HELLO WORLD`.

[[/images/fresh-runtime.svg|alt=Attempt 1 runs in container A and fails; Temporal retries in a fresh container B that can't see A's files; only B's output reaches Temporal, loom's Record and the Git store, which is outside every container]]

## Why

An agent step that writes files, installs packages or leaves processes behind shouldn't affect the next step, the next run, or its own retry. bobbin makes "start from clean" structural instead of a convention:

- **Fresh every time.** Every call, and every Temporal retry, is `podman run --rm` from the same pinned image. A failed attempt's half-written files can't leak into the retry.
- **Nothing from the host.** No volumes, no network, no host environment. Above all, the [warp](https://github.com/heedle-ai/warp/wiki) store holding agent memory is never reachable from inside a container: an agent's output reaches history only through loom's `Record` activity.
- **Reproducible inputs.** Images are referenced by content (an image ID or `name@sha256:` digest), never by a tag that can move.

bobbin reimplements nothing. It drives the `podman` command line, the way warp drives `git`, and Podman does the isolation with the kernel's namespaces, cgroups and overlay filesystem.

## Where bobbin fits

| Repo | Role |
|---|---|
| [weft](https://github.com/heedle-ai/weft/wiki) | Composition algebra |
| [draft](https://github.com/heedle-ai/draft/wiki) | Language: s-expressions that compile to shuttle plans |
| [shuttle](https://github.com/heedle-ai/shuttle/wiki) | Durable execution on Temporal |
| **bobbin** | A fresh container per agent step (this repo) |
| [reed](https://github.com/heedle-ai/reed/wiki) | A fresh browser per browsing step, reaching only the hosts its program names |
| [warp](https://github.com/heedle-ai/warp/wiki) | Content-addressed store |
| [drawdown](https://github.com/heedle-ai/drawdown/wiki) | Web UI |
| [loom](https://github.com/heedle-ai/loom/wiki) | Runs everything together |

## Pages

- [Getting Started](Getting-Started): install Podman, run the tests, run an agent
- [How It Works](How-It-Works): the exact Podman command, what each flag isolates, retries, errors, and what isn't isolated
- [Writing Agents](Writing-Agents): the input and output contract, and building a pinned image
- [Invariants](Invariants): every guarantee, the test or CI step that guards it, and where it came from
- [Contributing](Contributing)

## Requirements

- Go **1.24** or later
- Podman **4** or later on `PATH`. Tested with Podman 4.9.3. On macOS, Podman runs containers in a Linux VM (`podman machine`)
- Linux (amd64 or arm64), or macOS on Apple Silicon
