package bobbin_test

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"

	"github.com/heedle-ai/bobbin"
)

var zeros = strings.Repeat("0", 64)

type nopWorker struct{}

func (nopWorker) RegisterActivityWithOptions(any, activity.RegisterOptions) {}

// fakePodman is a shell script standing in for podman, so the runner's
// command line, environment and error handling are checked everywhere,
// including where podman isn't installed.
func fakePodman(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "podman")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestInv_ExactPodmanCommandLine: the flags are fixed, come before the
// image, and nothing a caller controls can add one. Arguments that look
// like flags come after the image, where podman passes them to the
// container's entrypoint.
func TestInv_ExactPodmanCommandLine(t *testing.T) {
	podman := fakePodman(t, `for a in "$@"; do printf '%s\n' "$a"; done`)
	s := bobbin.Spec{Image: zeros, Args: []string{"--volume=/:/host", "-v", "/:/h"},
		Env: map[string]string{"B": "2", "A": "--privileged"}}
	out, err := bobbin.Runner{Podman: podman}.Run(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"run", "--rm", "-i", "--pull=never", "--network=none", "--cap-drop=all",
		"--security-opt=no-new-privileges", "--pids-limit=256", "--memory=512m", "--log-driver=none",
		"--http-proxy=false", "--env", "A=--privileged", "--env", "B=2", zeros, "--volume=/:/host", "-v", "/:/h"}
	if got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n"); !reflect.DeepEqual(got, want) {
		t.Errorf("podman argv\n got  %q\n want %q", got, want)
	}
	if got := s.Argv(90 * time.Second); got[11] != "--timeout=90" {
		t.Errorf("with a deadline, argv[11] = %q, want --timeout=90", got[10])
	}
}

// TestInv_NoMounts: no code path can mount anything into a container,
// expose a port, add a device or share a host namespace. Every flag-like
// string literal in the package is on a fixed list, and Spec has no field
// that could carry a host path.
func TestInv_NoMounts(t *testing.T) {
	allowed := map[string]bool{"-i": true, "--rm": true, "--pull=never": true, "--network=none": true,
		"--cap-drop=all": true, "--security-opt=no-new-privileges": true, "--pids-limit=": true,
		"--memory=": true, "--log-driver=none": true, "--http-proxy=false": true, "--timeout=": true, "--env": true,
		// Sessions: run detached, named and labelled, with a shell; exec,
		// diff, rm and ps flags. None mounts, publishes or shares anything.
		"--detach": true, "--name": true, "--label": true, "--entrypoint": true, "-c": true,
		"--interactive": true, "--workdir": true, "--format": true, "--force": true, "--time": true,
		"--ignore": true,
		// Builds and pushes (build.go): fixed timestamps and labels, the
		// image ID and digest files, the Containerfile, the push format,
		// and TLS off only for a plain-HTTP registry the worker names.
		"--timestamp=0": true, "--identity-label=false": true, "--iidfile": true, "--file": true,
		"--digestfile": true, "--tls-verify=false": true}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			ast.Inspect(f, func(n ast.Node) bool {
				// CheckContainerfile reads Containerfile syntax (COPY
				// --from=…); it never builds a podman command line.
				if fn, ok := n.(*ast.FuncDecl); ok && fn.Name.Name == "CheckContainerfile" {
					return false
				}
				if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					s, _ := strconv.Unquote(lit.Value)
					if strings.HasPrefix(s, "-") && !allowed[s] {
						t.Errorf("%s: flag-like literal %q isn't on the allowed list", fset.Position(lit.Pos()), s)
					}
				}
				return true
			})
		}
	}
	var fields []string
	st := reflect.TypeOf(bobbin.Spec{})
	for i := 0; i < st.NumField(); i++ {
		fields = append(fields, st.Field(i).Name)
	}
	if got := strings.Join(fields, " "); got != "Image Args Env Stdin" {
		t.Errorf("Spec fields = %s; adding a field needs a new invariant", got)
	}
}

// TestInv_OnlyPodmanRunsProcesses: bobbin's own imports are an allowlist,
// and only podman.go and session.go start processes.
func TestInv_OnlyPodmanRunsProcesses(t *testing.T) {
	out, err := exec.Command("go", "list", "-f", `{{join .Imports "\n"}}`, ".").Output()
	if err != nil {
		t.Fatal(err)
	}
	ok := func(p string) bool {
		return !strings.Contains(strings.SplitN(p, "/", 2)[0], ".") || // standard library
			strings.HasPrefix(p, "go.temporal.io/sdk/") ||
			p == "github.com/heedle-ai/weft" || p == "github.com/heedle-ai/shuttle" ||
			p == "github.com/heedle-ai/shuttle/plan" || p == "github.com/heedle-ai/draft/registry"
	}
	for _, p := range strings.Fields(string(out)) {
		if !ok(p) {
			t.Errorf("bobbin imports %s", p)
		}
	}
	files, _ := filepath.Glob("*.go")
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") || name == "podman.go" || name == "session.go" {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range f.Imports {
			if p, _ := strconv.Unquote(im.Path.Value); p == "os/exec" || p == "syscall" {
				t.Errorf("%s imports %s; only podman.go and session.go start processes", name, p)
			}
		}
	}
}

// TestInv_ImagesArePinned: only content-addressed image references run.
func TestInv_ImagesArePinned(t *testing.T) {
	hex := "2301355972fe63bb4c16a24863f186d40e409bea9f92bb6f102a0a059205ada4"
	for _, img := range []string{hex, "sha256:" + hex, "localhost/agent@sha256:" + hex, "quay.io:443/org/agent@sha256:" + hex} {
		if err := (bobbin.Spec{Image: img}).Validate(); err != nil {
			t.Errorf("%s refused: %v", img, err)
		}
	}
	for _, img := range []string{"", "alpine", "alpine:3.20", "alpine:latest", hex[:12], strings.ToUpper(hex),
		"alpine@sha256:" + hex[:63], "alpine:3.20@sha256:", "-v", "--privileged", "agent@sha256:" + hex + "\n"} {
		if err := (bobbin.Spec{Image: img}).Validate(); !errors.Is(err, bobbin.ErrInvalid) {
			t.Errorf("%q accepted (err %v)", img, err)
		}
	}
	for _, s := range []bobbin.Spec{
		{Image: hex, Args: []string{"a\x00b"}}, {Image: hex, Args: []string{"\xff"}},
		{Image: hex, Env: map[string]string{"A=B": "1"}}, {Image: hex, Env: map[string]string{"A": "x\x00"}},
	} {
		if err := s.Validate(); !errors.Is(err, bobbin.ErrInvalid) {
			t.Errorf("%+v accepted", s)
		}
	}
}

// TestInv_ExitCodesAreClassified: podman's own failures (125, 126, 127
// with podman's "Error: " line) are ErrSetup; any other exit, including a
// step's own 125-127, is a retryable ExitError carrying the end of stderr,
// at most StderrTail bytes. (Review 4: a step exiting 125 was non-retryable.)
func TestInv_ExitCodesAreClassified(t *testing.T) {
	for _, c := range []struct {
		code  int
		last  string
		setup bool
	}{
		{1, "boom", false}, {3, "boom", false}, {7, "boom", false}, {137, "boom", false},
		{125, "boom", false}, {126, "boom", false}, {127, "boom", false},
		{125, "Error: image not known", true}, {126, "Error: permission denied", true}, {127, "Error: executable file not found", true},
	} {
		r := bobbin.Runner{Podman: fakePodman(t, "head -c 5000 /dev/zero | tr '\\0' x >&2; echo >&2; echo '"+c.last+"' >&2; exit "+strconv.Itoa(c.code))}
		_, err := r.Run(ctx, bobbin.Spec{Image: zeros})
		var exit *bobbin.ExitError
		switch {
		case c.setup && !errors.Is(err, bobbin.ErrSetup):
			t.Errorf("exit %d %q: %v, want ErrSetup", c.code, c.last, err)
		case !c.setup && (!errors.As(err, &exit) || exit.Code != c.code || len(exit.Stderr) > bobbin.StderrTail || !strings.HasSuffix(exit.Stderr, c.last)):
			t.Errorf("exit %d %q: %v, want ExitError with the stderr tail", c.code, c.last, err)
		}
	}
}

// TestInv_PodmanIgnoresInheritedConfiguration: variables that would point
// podman at another machine, storage or configuration are dropped, and its
// messages are in the C locale.
func TestInv_PodmanIgnoresInheritedConfiguration(t *testing.T) {
	t.Setenv("CONTAINER_HOST", "ssh://elsewhere")
	t.Setenv("CONTAINERS_CONF", "/tmp/other.conf")
	t.Setenv("PODMAN_CONNECTIONS_CONF", "/tmp/x")
	t.Setenv("LANG", "de_DE.UTF-8")
	out, err := bobbin.Runner{Podman: fakePodman(t, "env")}.Run(ctx, bobbin.Spec{Image: zeros})
	if err != nil {
		t.Fatal(err)
	}
	env := "\n" + string(out)
	for _, bad := range []string{"\nCONTAINER_HOST=", "\nCONTAINERS_CONF=", "\nPODMAN_CONNECTIONS_CONF=", "\nLANG="} {
		if strings.Contains(env, bad) {
			t.Errorf("podman inherited %s", strings.TrimSpace(bad))
		}
	}
	if !strings.Contains(env, "\nLC_ALL=C\n") {
		t.Error("LC_ALL=C not set")
	}
}

// TestInv_OutputIsBounded: standard output beyond MaxOutput stops the
// container with ErrOutputTooLarge instead of filling the worker's memory
// and Temporal's history; standard error keeps only its last StderrTail
// bytes however much is written.
func TestInv_OutputIsBounded(t *testing.T) {
	big := bobbin.Runner{Podman: fakePodman(t, "head -c "+strconv.Itoa(bobbin.MaxOutput+1)+" /dev/zero")}
	if _, err := big.Run(ctx, bobbin.Spec{Image: zeros}); !errors.Is(err, bobbin.ErrOutputTooLarge) {
		t.Errorf("MaxOutput+1 bytes: err = %v", err)
	}
	exact := bobbin.Runner{Podman: fakePodman(t, "head -c "+strconv.Itoa(bobbin.MaxOutput)+" /dev/zero")}
	if out, err := exact.Run(ctx, bobbin.Spec{Image: zeros}); err != nil || len(out) != bobbin.MaxOutput {
		t.Errorf("MaxOutput bytes: %d, %v", len(out), err)
	}
	noisy := bobbin.Runner{Podman: fakePodman(t, "head -c 10000000 /dev/zero | tr '\\0' x >&2; echo end >&2; exit 1")}
	_, err := noisy.Run(ctx, bobbin.Spec{Image: zeros})
	var exit *bobbin.ExitError
	if !errors.As(err, &exit) || len(exit.Stderr) > bobbin.StderrTail || !strings.HasSuffix(exit.Stderr, "end") {
		t.Errorf("10 MB of stderr: %v", err)
	}
}

// TestInv_ExactSessionCommandLines: a session's container is started with
// the same hardening as a single run (no network, no capabilities, limits,
// nothing mounted), named and labelled so it can be found and swept, with
// /bin/sh doing nothing until it's removed; commands, writes and reads go
// through podman exec with every path an argument, never part of a
// command; Close removes it, whatever happened.
func TestInv_ExactSessionCommandLines(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log")
	podman := fakePodman(t, `printf '%s\n' "$*" >> `+log+`; case "$1" in exec) cat > /dev/null; echo out; echo err >&2;; esac`)
	s := bobbin.Spec{Image: zeros, Env: map[string]string{"HEEDLE_NODE": "n0"}}
	ss, err := bobbin.Runner{Podman: podman}.Start(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	name := ss.Name()
	if !regexp.MustCompile(`^bobbin-session-[0-9a-f]{16}$`).MatchString(name) {
		t.Fatalf("session name %q", name)
	}
	res, err := ss.Exec(ctx, "ls -la", []byte("in"))
	if err != nil || res.Stdout != "out\n" || res.Stderr != "err\n" || res.ExitCode != 0 {
		t.Errorf("exec = %+v, %v", res, err)
	}
	if err := ss.Write(ctx, "/in/n0.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := ss.Write(ctx, "relative", nil); !errors.Is(err, bobbin.ErrInvalid) {
		t.Errorf("relative write: %v", err)
	}
	ss.Close()
	b, _ := os.ReadFile(log)
	want := "run --detach --rm --name " + name + " --label bobbin.session=1 --pull=never --network=none --cap-drop=all " +
		"--security-opt=no-new-privileges --pids-limit=256 --memory=512m --log-driver=none --http-proxy=false --timeout=1800 " +
		"--entrypoint /bin/sh --env HEEDLE_NODE=n0 " + zeros + " -c trap 'exit 0' TERM; while :; do sleep 3600 & wait $!; done\n" +
		"exec " + name + " /bin/sh -c mkdir -p /work\n" +
		"exec --interactive --workdir /work " + name + " /bin/sh -c ls -la sh\n" +
		"exec --interactive --workdir /work " + name + ` /bin/sh -c mkdir -p "$(dirname "$1")" && cat > "$1" sh /in/n0.txt` + "\n" +
		"rm --force --time 0 --ignore " + name + "\n"
	if string(b) != want {
		t.Errorf("podman calls:\n%s\nwant\n%s", b, want)
	}
	if _, err := (bobbin.Runner{Podman: podman}).Start(ctx, bobbin.Spec{Image: zeros, Args: []string{"x"}}); !errors.Is(err, bobbin.ErrInvalid) {
		t.Errorf("a session with arguments: %v", err)
	}
}

// TestInv_SessionOutputIsBounded: each of a command's outputs keeps at
// most MaxExecOutput bytes and says it was cut; a read past its limit is
// an error, never a short read; and only podman's own last line on
// standard error makes a failed command a setup error.
func TestInv_SessionOutputIsBounded(t *testing.T) {
	podman := fakePodman(t, `case "$1" in exec) head -c 200000 /dev/zero; head -c 70000 /dev/zero >&2;; esac`)
	ss, err := bobbin.Runner{Podman: podman}.Start(ctx, bobbin.Spec{Image: zeros})
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	res, err := ss.Exec(ctx, "x", nil)
	if err != nil || len(res.Stdout) != bobbin.MaxExecOutput || len(res.Stderr) != bobbin.MaxExecOutput || !res.Truncated {
		t.Errorf("exec: %d and %d bytes, truncated %v, %v", len(res.Stdout), len(res.Stderr), res.Truncated, err)
	}
	if _, err := ss.Read(ctx, "/work/big", 1000); !errors.Is(err, bobbin.ErrOutputTooLarge) {
		t.Errorf("read past its limit: %v", err)
	}
	// 100 KiB of standard error ending in podman's error line: the cut head
	// doesn't hold it, but it's still recognised.
	broken := fakePodman(t, `case "$*" in *"mkdir -p /work"*) exit 0;; esac
case "$1" in exec) head -c 100000 /dev/zero | tr '\\0' x >&2; echo >&2; echo "Error: no such container" >&2; exit 125;; esac`)
	bs, err := bobbin.Runner{Podman: broken}.Start(ctx, bobbin.Spec{Image: zeros})
	if err != nil {
		t.Fatal(err)
	}
	defer bs.Close()
	if _, err := bs.Exec(ctx, "x", nil); !errors.Is(err, bobbin.ErrSetup) {
		t.Errorf("podman's error after long stderr: %v", err)
	}
}
