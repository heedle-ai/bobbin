# bobbin

heedle-ai's agent runtime: every agent step runs in a brand-new Podman container, from a pinned image, with no network and nothing from the host mounted. When the step finishes, the container and everything it wrote are gone; only its output comes back.

```lisp
(pipe (par (source "weather") (source "carrier"))
      (container "sha256:2301355972fe63bb4c16a24863f186d40e409bea9f92bb6f102a0a059205ada4" "--claim" "late"))
```

```go
out, err := bobbin.Runner{}.Run(ctx, bobbin.Spec{Image: id, Args: []string{"hello"}, Stdin: input})
```

bobbin reimplements nothing: it drives the `podman` command line, as warp drives `git`. When Temporal retries a failed step, the retry gets a fresh container too, so a half-finished attempt can't leak into the result.

Documentation: **[wiki](https://github.com/heedle-ai/bobbin/wiki)**

Requires Go 1.24+ and Podman 4 or later (Linux, or `podman machine` on macOS).

```bash
go test -race ./...
```
