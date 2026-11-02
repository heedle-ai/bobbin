package bobbin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// Errors returned by Run. Test with errors.Is.
var (
	// ErrInvalid: the Spec can never run (unpinned image, bad argument or
	// environment). Retrying won't help.
	ErrInvalid = errors.New("bobbin: invalid spec")
	// ErrSetup: Podman couldn't start the container (podman missing, image
	// not present locally, command not found in the image). Podman exits
	// 125, 126 or 127 for these. Retrying won't help.
	ErrSetup = errors.New("bobbin: container setup failed")
	// ErrOutputTooLarge: the container wrote more than MaxOutput bytes to
	// standard output. It was stopped; a retry would do the same.
	ErrOutputTooLarge = errors.New("bobbin: output too large")
)

// ExitError is a container whose command ran and exited non-zero. It may
// be transient, so it is retryable: a retry gets a fresh container.
type ExitError struct {
	Code   int
	Stderr string // the last StderrTail bytes of standard error
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("bobbin: container exited %d: %s", e.Code, e.Stderr)
}

// StderrTail is how much of a failed container's standard error is kept.
const StderrTail = 2048

// MaxOutput is the most standard output a container may write. An output
// becomes an activity result in Temporal's history, which is meant for
// small values; a step that produces something large should store it
// elsewhere and output a reference.
const MaxOutput = 1 << 20

// Limits applied to every container.
const (
	MemoryLimit = "512m"
	PidsLimit   = 256
)

// Spec is one container run. There is deliberately no field for volumes,
// mounts, ports, devices or network: a container sees only its image,
// Args, Env and Stdin.
type Spec struct {
	// Image must be pinned to content: a full 64-hex image ID (optionally
	// "sha256:"-prefixed) or name@sha256:<64 hex>. Tags and short IDs are
	// refused, because what they name can change.
	Image string
	// Args are passed to the image's entrypoint.
	Args []string
	// Env sets environment variables inside the container. Nothing is
	// inherited from the host.
	Env map[string]string
	// Stdin is the container's standard input.
	Stdin []byte
}

// Runner runs containers with the podman command line.
type Runner struct {
	// Podman is the podman executable; empty means "podman" on PATH.
	Podman string
}

var (
	pinned = regexp.MustCompile(`^(sha256:)?[0-9a-f]{64}$|^[a-z0-9][a-z0-9._/:-]*@sha256:[0-9a-f]{64}$`)
	envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Validate reports whether s can run, wrapping ErrInvalid if not.
func (s Spec) Validate() error {
	if !pinned.MatchString(s.Image) {
		return fmt.Errorf("%w: image %q must be pinned: a full 64-hex image ID or name@sha256:<digest>", ErrInvalid, s.Image)
	}
	for _, a := range s.Args {
		if strings.ContainsRune(a, 0) || !utf8.ValidString(a) {
			return fmt.Errorf("%w: argument %q must be valid UTF-8 without NUL", ErrInvalid, a)
		}
	}
	for k, v := range s.Env {
		if !envKey.MatchString(k) || strings.ContainsRune(v, 0) || !utf8.ValidString(v) {
			return fmt.Errorf("%w: environment variable %q=%q", ErrInvalid, k, v)
		}
	}
	return nil
}

// Argv is the exact podman command line Run uses for s, without the
// executable. Flags come first and are fixed; the image, then s.Args,
// come last, so nothing in Args can be read as a podman flag.
func (s Spec) Argv(timeout time.Duration) []string {
	argv := []string{
		"run", "--rm", "-i",
		"--pull=never",                     // only images already present: no surprise downloads
		"--network=none",                   // no network at all
		"--cap-drop=all",                   // no Linux capabilities
		"--security-opt=no-new-privileges", // setuid binaries can't gain any
		"--pids-limit=" + strconv.Itoa(PidsLimit),
		"--memory=" + MemoryLimit,
		"--log-driver=none",  // output goes only to us
		"--http-proxy=false", // don't copy the host's proxy variables in
	}
	if timeout > 0 {
		argv = append(argv, "--timeout="+strconv.Itoa(int(math.Ceil(timeout.Seconds()))))
	}
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		argv = append(argv, "--env", k+"="+s.Env[k])
	}
	argv = append(argv, s.Image)
	return append(argv, s.Args...)
}

// Run starts a fresh container for s, feeds it s.Stdin, waits for it and
// returns its standard output. The container is removed afterwards
// (--rm), so nothing it wrote survives. If ctx has a deadline, Podman
// also stops the container at that deadline (--timeout); if ctx is
// cancelled, podman is sent SIGTERM, which it passes to the container.
func (r Runner) Run(ctx context.Context, s Spec) ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	var timeout time.Duration
	if d, ok := ctx.Deadline(); ok {
		if timeout = time.Until(d); timeout <= 0 {
			return nil, ctx.Err()
		}
	}
	podman := r.podman()
	cmd := exec.CommandContext(ctx, podman, s.Argv(timeout)...)
	cmd.Env = cleanEnv(os.Environ())
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	cmd.Stdin = bytes.NewReader(s.Stdin)
	stdout := &capped{max: MaxOutput, onFull: func() { _ = cmd.Process.Signal(syscall.SIGTERM) }}
	stderr := &tail{max: StderrTail}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	if stdout.full {
		return nil, fmt.Errorf("%w: more than %d bytes", ErrOutputTooLarge, MaxOutput)
	}
	if err == nil {
		return stdout.buf.Bytes(), nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	tail := strings.TrimSpace(string(stderr.buf))
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return nil, fmt.Errorf("%w: %v", ErrSetup, err) // podman couldn't be started at all
	}
	// Podman uses 125 (its own error), 126 (can't run the command) and 127
	// (command not found), and ends with an "Error: " line. A step can exit
	// with those codes too; without podman's message, it's the step's own
	// exit, and retryable.
	code := exit.ExitCode()
	if (code == 125 || code == 126 || code == 127) && podmanError(tail) {
		return nil, fmt.Errorf("%w: podman exited %d: %s", ErrSetup, code, tail)
	}
	return nil, &ExitError{Code: code, Stderr: tail}
}

// podmanError reports whether stderr ends with podman's own error line.
func podmanError(stderr string) bool {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	return strings.HasPrefix(lines[len(lines)-1], "Error: ")
}

// cleanEnv drops variables that could point podman somewhere else (a
// remote connection, other storage or configuration) or change the
// language of its messages.
func cleanEnv(env []string) []string {
	var out []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "CONTAINER") || strings.HasPrefix(k, "PODMAN") ||
			strings.HasPrefix(k, "LC_") || strings.HasPrefix(k, "LANG") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "LC_ALL=C")
}

// capped keeps up to max bytes; past that it records that it was full,
// calls onFull once, and discards the rest.
type capped struct {
	buf    bytes.Buffer
	max    int
	full   bool
	onFull func()
}

func (c *capped) Write(p []byte) (int, error) {
	if c.full {
		return len(p), nil
	}
	if c.buf.Len()+len(p) > c.max {
		c.full = true
		c.onFull()
		return len(p), nil
	}
	return c.buf.Write(p)
}

// tail keeps the last max bytes written to it.
type tail struct {
	buf []byte
	max int
}

func (t *tail) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

// Build builds b's image and returns its ID (sha256:<hex>). The build
// context is a fresh temporary directory holding only the Containerfile
// and in/<node>.txt for each input, removed afterwards; podman runs as
// BuildArgv says. A Containerfile CheckContainerfile refuses wraps
// ErrInvalid; a base image not on the worker wraps ErrSetup; a build that
// fails is an *ExitError.
func (r Runner) Build(ctx context.Context, b Build) (string, error) {
	if err := CheckContainerfile(b.Containerfile); err != nil {
		return "", err
	}
	ids, err := sortedInputs(b.Inputs)
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "bobbin-build-")
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrSetup, err)
	}
	defer os.RemoveAll(dir)
	ctxDir := dir + "/context"
	if err := os.MkdirAll(ctxDir+"/in", 0o755); err != nil {
		return "", fmt.Errorf("%w: %v", ErrSetup, err)
	}
	if err := os.WriteFile(ctxDir+"/Containerfile", []byte(b.Containerfile), 0o644); err != nil {
		return "", fmt.Errorf("%w: %v", ErrSetup, err)
	}
	for _, id := range ids {
		if err := os.WriteFile(ctxDir+"/in/"+id+".txt", []byte(b.Inputs[id]), 0o644); err != nil {
			return "", fmt.Errorf("%w: %v", ErrSetup, err)
		}
	}
	iid := dir + "/iid"
	cmd := exec.CommandContext(ctx, r.podman(), BuildArgv(ctxDir, iid)...)
	cmd.Env = cleanEnv(os.Environ())
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	stderr := &tail{max: StderrTail}
	cmd.Stdout, cmd.Stderr = stderr, stderr // the build log: its end says what failed
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		log := strings.TrimSpace(string(stderr.buf))
		var exit *exec.ExitError
		switch {
		case !errors.As(err, &exit):
			return "", fmt.Errorf("%w: %v", ErrSetup, err)
		case strings.Contains(log, "image not known"):
			return "", fmt.Errorf("%w: a base image isn't on the worker (builds never pull): %s", ErrSetup, log)
		}
		return "", &ExitError{Code: exit.ExitCode(), Stderr: log}
	}
	id, err := os.ReadFile(iid)
	if err != nil || !imageID.Match(bytes.TrimSpace(id)) {
		return "", fmt.Errorf("bobbin: podman build wrote no image ID: %q", id)
	}
	return string(bytes.TrimSpace(id)), nil
}

// Push pushes image id to reg, as PushArgv says, with the worker's own
// podman login, and returns where it is: repo@sha256:<manifest digest>.
func (r Runner) Push(ctx context.Context, id string, reg ImageRegistry) (string, error) {
	if !imageID.MatchString(id) || !imageRepo.MatchString(reg.Repo) {
		return "", fmt.Errorf("%w: pushing %q to %q", ErrInvalid, id, reg.Repo)
	}
	dir, err := os.MkdirTemp("", "bobbin-push-")
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrSetup, err)
	}
	defer os.RemoveAll(dir)
	cmd := exec.CommandContext(ctx, r.podman(), PushArgv(id, reg.Repo, dir+"/digest", reg.PlainHTTP)...)
	cmd.Env = cleanEnv(os.Environ())
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	stderr := &tail{max: StderrTail}
	cmd.Stdout, cmd.Stderr = stderr, stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("bobbin: podman push: %v: %s", err, strings.TrimSpace(string(stderr.buf)))
	}
	d, err := os.ReadFile(dir + "/digest")
	if err != nil || !imageID.Match(bytes.TrimSpace(d)) {
		return "", fmt.Errorf("bobbin: podman push wrote no digest: %q", d)
	}
	return reg.Repo + "@" + string(bytes.TrimSpace(d)), nil
}
