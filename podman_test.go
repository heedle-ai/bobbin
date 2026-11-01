package bobbin_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/heedle-ai/bobbin"
)

var ctx = context.Background()

var (
	imagesMu sync.Mutex
	images   = map[string]string{} // package path -> image ID
	podmanOK = sync.OnceValue(func() error { return exec.Command("podman", "info").Run() })
)

// testImage builds the test image once per test binary: FROM scratch plus
// the static agent in testdata/agent.
func testImage(t *testing.T) string { return buildImage(t, "./testdata/agent", "agent") }

// buildImage compiles the Go command at pkg as a static Linux binary named
// bin and builds a FROM scratch image around it with podman, once per test
// binary. Without a working podman the container tests skip, unless
// BOBBIN_REQUIRE_PODMAN=1 (set in CI), where they fail instead.
func buildImage(t *testing.T, pkg, bin string) string {
	t.Helper()
	if err := podmanOK(); err != nil {
		if os.Getenv("BOBBIN_REQUIRE_PODMAN") == "1" {
			t.Fatal("podman isn't usable here (podman info failed):", err)
		}
		t.Skip("podman isn't usable here (podman info failed); install podman to run container tests:", err)
	}
	imagesMu.Lock()
	defer imagesMu.Unlock()
	if id, ok := images[pkg]; ok {
		return id
	}
	dir := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(dir, bin), pkg)
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, out)
	}
	cf := fmt.Sprintf("FROM scratch\nCOPY %s /%s\nENTRYPOINT [\"/%s\"]\n", bin, bin, bin)
	if err := os.WriteFile(filepath.Join(dir, "Containerfile"), []byte(cf), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("podman", "build", "--quiet", dir).Output()
	if err != nil {
		t.Fatalf("podman build: %v", err)
	}
	lines := strings.Fields(string(out))
	images[pkg] = lines[len(lines)-1]
	return images[pkg]
}

func run(t *testing.T, s bobbin.Spec) string {
	t.Helper()
	out, err := bobbin.Runner{}.Run(ctx, s)
	if err != nil {
		t.Fatalf("Run(%v): %v", s.Args, err)
	}
	return string(out)
}

// Arguments, standard input and environment reach the container exactly,
// and standard output comes back.
func TestContainer_RoundTrip(t *testing.T) {
	img := testImage(t)
	got := run(t, bobbin.Spec{Image: img, Args: []string{"echo", "a b", "--volume=/:/host", "ü"},
		Stdin: []byte(`{"n0":"hi"}`), Env: map[string]string{"HEEDLE_NODE": "n1", "HEEDLE_ATTEMPT": "1"}})
	want := `args=["a b" "--volume=/:/host" "ü"] stdin={"n0":"hi"} node=n1 attempt=1` + "\n"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// Every run is a fresh container: what one run writes, the next can't see.
func TestContainer_FreshLayerEachRun(t *testing.T) {
	img := testImage(t)
	for i := 0; i < 3; i++ {
		if got := run(t, bobbin.Spec{Image: img, Args: []string{"scratch"}}); got != "clean\n" {
			t.Fatalf("run %d saw %q: an earlier container's writes survived", i+1, got)
		}
	}
}

// Containers have no network: only a loopback interface, and dialling out
// fails.
func TestContainer_NoNetwork(t *testing.T) {
	img := testImage(t)
	if got := run(t, bobbin.Spec{Image: img, Args: []string{"net"}}); got != "interfaces: lo\nnetwork: unreachable\n" {
		t.Errorf("got %q", got)
	}
}

// Nothing from the host's environment reaches the container.
func TestContainer_NoHostEnvironment(t *testing.T) {
	img := testImage(t)
	t.Setenv("BOBBIN_HOST_SECRET", "s3cret")
	got := run(t, bobbin.Spec{Image: img, Args: []string{"env"}, Env: map[string]string{"MINE": "1"}})
	if strings.Contains(got, "BOBBIN_HOST_SECRET") || !strings.Contains(got, "MINE") {
		t.Errorf("container environment: %q", got)
	}
}

// A command that fails is a retryable ExitError with its stderr; an image
// that isn't present is ErrSetup.
func TestContainer_ErrorsAreClassified(t *testing.T) {
	img := testImage(t)
	_, err := bobbin.Runner{}.Run(ctx, bobbin.Spec{Image: img, Args: []string{"fail"}})
	var exit *bobbin.ExitError
	if !errors.As(err, &exit) || exit.Code != 7 || !strings.Contains(exit.Stderr, "something went wrong") {
		t.Errorf("failing command: %v", err)
	}
	_, err = bobbin.Runner{}.Run(ctx, bobbin.Spec{Image: "localhost/nothing@sha256:" + strings.Repeat("0", 64)})
	if !errors.Is(err, bobbin.ErrSetup) || !strings.Contains(err.Error(), "image not known") {
		t.Errorf("missing image: %v", err)
	}
}
