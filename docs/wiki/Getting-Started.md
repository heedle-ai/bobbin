## 1. Install Go 1.24+ and Podman

**macOS (Apple Silicon).** Podman runs containers in a small Linux VM; create and start it once:

```bash
brew install go gh podman
podman machine init
podman machine start
podman info --format '{{.Host.Arch}}'    # arm64
```

**Linux (amd64 or arm64)**

```bash
sudo apt-get install -y gh podman  # or: sudo dnf install -y gh podman
podman --version                   # podman version 4.9.3 or later
```

Install Go 1.24+ from [go.dev/dl](https://go.dev/dl/) or your package manager (`brew install go`). Rootless Podman works; you don't need `sudo` to run containers.

## 2. Run the tests

bobbin builds against sibling checkouts of the other heedle-ai repos:

```bash
gh auth login        # once; bobbin is private
gh auth setup-git    # lets git clone them with your gh login
mkdir -p ~/src/heedle-ai && cd ~/src/heedle-ai
for r in weft draft shuttle bobbin; do git clone https://github.com/heedle-ai/$r.git; done
cd bobbin
go test -race ./...
```

The `TestContainer_*` tests build a small test image with Podman and run real containers. Without a working Podman they skip and say why; set `BOBBIN_REQUIRE_PODMAN=1` to make them fail instead, as CI does on Linux:

```bash
BOBBIN_REQUIRE_PODMAN=1 go test -count=1 -run 'TestContainer_' -v .
```

## 3. Build the example agent

[`examples/upper`](https://github.com/heedle-ai/bobbin/tree/main/examples/upper) reads its upstream outputs and prints them upper-cased. Build it as a static Linux binary for the machine Podman runs on, then wrap it in an image `FROM scratch`:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=$(go env GOARCH) go build -o examples/upper/upper ./examples/upper
IMAGE=$(podman build --quiet examples/upper)
echo "$IMAGE"
```

```text
5c5f488453940eab6dedebec814e34eb36317689cae82226a8bc606bb56d0642
```

The ID is the image's content address. Yours may differ (it depends on the Go version that built the binary); use the one you got.

## 4. Run it the way bobbin does

```bash
echo '{"n0":"hello","n1":"world"}' | podman run --rm -i --pull=never --network=none "$IMAGE"
```

```text
HELLO WORLD
```

bobbin adds more flags than that (see [How It Works](How-It-Works)), but that's the idea: a new container per call, input on stdin, output on stdout.

## 5. Use it in a program

Register the `container` command with draft and the activity with a Temporal worker:

```go
reg := draft.DefaultRegistry()
if err := bobbin.Register(reg, w, bobbin.Runner{}); err != nil { // w is a worker.Worker
	return err
}
plan.Register(w)
```

Then `(container "<image ID>" args...)` is a step like any other:

```lisp
(pipe (par (echo "hello") (echo "world"))
      (container "5c5f488453940eab6dedebec814e34eb36317689cae82226a8bc606bb56d0642"))
```

`TestContainer_ExampleAgent` builds `examples/upper` and runs exactly this program through shuttle's workflow; its last step outputs `HELLO WORLD`.
