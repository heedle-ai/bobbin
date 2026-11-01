## Ground rules

- **Reuse Podman, never reimplement it.** Every container operation is a `podman` command. If something seems to need the container engine's API, namespaces or cgroups directly, stop and ask first.
- **Nothing from the host goes in.** Never add a volume, mount, port, device, host namespace or inherited environment, nor a `Spec` field that could carry one. `TestInv_NoMounts` fails if you do; changing it needs a new invariant and a review.
- **A new container every time.** Never reuse a container between calls or attempts.
- **A mistake isn't fixed until it has a row on [Invariants](Invariants)** and a guard that fails on the old code. CI checks the page against the tests in both directions.
- **Tests that need Podman are `TestContainer_*`.** They skip without Podman, so they're guarded on the Invariants page by `ci:containers`, the CI step that runs them on Linux and fails if any skip. Everything else uses a fake `podman` script and runs everywhere.
- **Update this wiki in the same change.** The pages live in `docs/wiki/` and publish on every push to `main`.
- Supported platforms are Linux (amd64, arm64) and macOS on Apple Silicon.

## Before opening a pull request

```bash
gofmt -l .        # prints nothing
go vet ./...
go test -race ./...
BOBBIN_REQUIRE_PODMAN=1 go test -count=1 -run 'TestContainer_' .
bash .github/invariants.sh
```
