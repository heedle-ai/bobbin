Every invariant below is guarded by a test or a CI step, and CI checks this page against the code on every push with `bash .github/invariants.sh`. Only the **Guarded by** column counts. Each test named there must exist in exactly one package and is run, and the gate fails if it fails or skips; each `ci:NAME` must be a step in `.github/workflows/ci.yml`. Every `TestLaw_*`, `TestInv_*` and `Fuzz*` test must be listed, and no test file may have build tags, which would hide tests from CI. A mistake isn't fixed until it has a row on this page.

Tests that need Podman (`TestContainer_*`) skip where it isn't installed, so they're guarded by `ci:containers`: the CI step that runs them on Linux with `BOBBIN_REQUIRE_PODMAN=1` and fails if any skip.

[[/images/invariant-loop.svg|alt=From a mistake to a codified invariant: mistake, named invariant, guard, CI gate]]

[[/images/fresh-runtime.svg|alt=A fresh container for every attempt; only the output leaves; the Git store is outside every container]]

| ID | Invariant | Guarded by | Came from |
|---|---|---|---|
| BOB-1 | Every run is a brand-new container: what one run writes, the next can't see (`TestContainer_FreshLayerEachRun`) | `ci:containers` | Design: a brand-new runtime for each agent |
| BOB-2 | A Temporal retry runs in a fresh container, so a failed attempt's files never reach the result (`TestContainer_RetryRunsInAFreshContainer`) | `ci:containers` | Design: retries must start from clean |
| BOB-3 | Containers have no network: only a loopback interface, and dialling out fails (`TestContainer_NoNetwork`) | `ci:containers` | Removing `--network=none` gave the container `eth0`; the first version of this test only dialled out, which a sandbox without internet also blocks |
| BOB-4 | Nothing from the host's environment reaches a container (`TestContainer_NoHostEnvironment`) | `ci:containers` | Design |
| BOB-5 | Podman's command line is exact: fixed flags, all before the image, so no argument or variable can become a Podman flag | `TestInv_ExactPodmanCommandLine` | Design: arguments come from programs |
| BOB-6 | Nothing from the host can be mounted, published or shared: every flag in the package is on a fixed list, and `Spec` has only Image, Args, Env and Stdin | `TestInv_NoMounts` | Design: the warp store must never be reachable from a container |
| BOB-7 | Images are pinned by content: a full image ID or `name@sha256:` digest; tags, short IDs and malformed references are refused before Podman runs | `TestInv_ImagesArePinned` | Design: a record must say exactly what ran |
| BOB-8 | Podman's own failures (exit 125, 126 or 127 ending with its `Error: ` line, or no podman) are `ErrSetup`; any other exit, including a step's own 125–127, is a retryable `ExitError` with the last 2 KiB of stderr; a missing image is `ErrSetup` | `TestInv_ExitCodesAreClassified`, `ci:containers` | Review 4: a step exiting 125 was classified as a setup failure and never retried |
| BOB-9 | In a program, invalid specs and setup failures fail at once with the non-retryable types `InvalidInput` and `ContainerSetup` | `TestInv_CommandErrorsAreNonRetryable` | Design |
| BOB-10 | Output is bounded: more than 1 MiB on stdout stops the container with `ErrOutputTooLarge` (`OutputTooLarge`, non-retryable); stderr keeps only its last 2 KiB however much is written | `TestInv_OutputIsBounded` | Writing the docs: an unbounded output would fill the worker's memory and Temporal's history |
| BOB-11 | `podman` ignores inherited configuration (`CONTAINER*`, `PODMAN*`) and runs in the C locale | `TestInv_PodmanIgnoresInheritedConfiguration` | Design, as warp ignores `GIT_*` variables |
| BOB-12 | bobbin's imports are an allowlist, and only `podman.go` starts processes | `TestInv_OnlyPodmanRunsProcesses` | Reuse rule: one door to the container engine |
| BOB-13 | `(container IMAGE ARG...)` takes an image and any number of arguments, and compiles to the `Container` activity | `TestInv_DraftCommandShape` | Design |
| BOB-14 | Upstream outputs arrive on stdin as JSON keyed by node ID, with `HEEDLE_NODE` and `HEEDLE_ATTEMPT` set, and the documented example agent's output is exact (`TestContainer_RoundTrip`, `TestContainer_InAProgram`, `TestContainer_ExampleAgent`) | `ci:containers` | Docs must never claim untested behaviour |
| BOB-15 | This page is checked strictly against the code: every listed guard exists once and runs without skipping, every `TestLaw_`, `TestInv_` and `Fuzz` test is listed, and no test file has build tags | `ci:invariants` | Review 3 of the other repos: a reviewer satisfied the old gate with a guard named only in prose, a skipped test and a build-tagged test |
| BOB-16 | The host's proxy variables never reach a container (`--http-proxy=false`) | `TestInv_ExactPodmanCommandLine`, `ci:containers` | Review 4: Podman copies them in by default, credentials included |
| BOB-17 | Endless output really stops the container: `ErrOutputTooLarge` promptly, and no container left running (checked with real Podman) | `ci:containers` | Review 4: the fake podman stopped by itself, so removing the stop passed every test |
