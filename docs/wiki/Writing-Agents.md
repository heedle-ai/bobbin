An agent is any program in a container image. bobbin runs it once per step, in a fresh container.

## The contract

| | |
|---|---|
| **Arguments** | `(container IMAGE ARG...)` passes each `ARG` to the image's entrypoint, unchanged |
| **Standard input** | A JSON object: each upstream node ID mapped to its output, keys sorted. `{}` when the step has no upstream |
| **Environment** | `HEEDLE_NODE`: this step's node ID (`n2`). `HEEDLE_ATTEMPT`: Temporal's attempt number, from 1. Nothing from the host |
| **Standard output** | The step's output, at most 1 MiB |
| **Exit code** | 0 for success. Any other code except 125–127 fails the step, and Temporal retries it in a fresh container |
| **Filesystem** | The image, plus an empty writable layer that is deleted afterwards |
| **Network** | None |

So for `(pipe (par (echo "hello") (echo "world")) (container IMAGE))`, the agent receives:

```json
{"n0":"hello","n1":"world"}
```

## The example agent

[`examples/upper/main.go`](https://github.com/heedle-ai/bobbin/blob/main/examples/upper/main.go):

```go
func main() {
	var upstream map[string]string // node ID -> that node's output
	if err := json.NewDecoder(os.Stdin).Decode(&upstream); err != nil {
		fmt.Fprintln(os.Stderr, "upper:", err)
		os.Exit(1) // non-zero: the step fails, and Temporal retries it in a fresh container
	}
	ids := make([]string, 0, len(upstream))
	for id := range upstream {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var parts []string
	for _, id := range ids {
		parts = append(parts, strings.ToUpper(upstream[id]))
	}
	fmt.Println(strings.Join(parts, " "))
}
```

[`examples/upper/Containerfile`](https://github.com/heedle-ai/bobbin/blob/main/examples/upper/Containerfile):

```dockerfile
FROM scratch
COPY upper /upper
ENTRYPOINT ["/upper"]
```

`TestContainer_ExampleAgent` builds this image and runs it in a program; the output is `HELLO WORLD`.

## Building and pinning an image

Build a static binary for Linux on the machine's architecture (`arm64` on Apple Silicon, where Podman's VM is arm64), then the image:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=$(go env GOARCH) go build -o examples/upper/upper ./examples/upper
IMAGE=$(podman build --quiet examples/upper)
```

`podman build --quiet` prints the image ID, the image's content address. Use it in programs:

```lisp
(container "5c5f488453940eab6dedebec814e34eb36317689cae82226a8bc606bb56d0642")
```

For an image from a registry, pull it once and use its digest, `name@sha256:<digest>`:

```bash
podman image inspect --format '{{.Digest}}' IMAGE
```

Tags are refused: `alpine:3.20` can point at different content tomorrow, and a record should say exactly what ran.

## Good habits

- **Start `FROM scratch`** with a static binary when you can. The smaller the image, the less there is to trust.
- **Be idempotent.** A step can run more than once (retries), each time from clean.
- **Keep outputs small.** Store large results elsewhere and output a reference.
- **Write diagnostics to stderr.** On failure, its last 2 KiB are in the error Temporal records.
