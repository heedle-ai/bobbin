package bobbin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
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
	// Secrets also set environment variables inside the container, but
	// their values never appear on podman's command line (where any
	// process on the host could read them): the command line names them
	// (--env NAME), and podman takes each value from its own environment,
	// which bobbin sets. Nothing else is inherited from the host.
	Secrets map[string]string
	// Stdin is the container's standard input.
	Stdin []byte
	// Labels are heedle's labels for the container (roadmap 9.2): names,
	// never anything from the host. With no Kind, a one-shot run is a
	// "container" and a session an "agent"; with no run labels, those
	// the context carries (WithLabels) are used.
	Labels Labels
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
	if err := s.Labels.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	for k, v := range s.Secrets {
		if _, both := s.Env[k]; both || !envKey.MatchString(k) || strings.ContainsRune(v, 0) || !utf8.ValidString(v) || v == "" {
			return fmt.Errorf("%w: secret %q must be a valid, non-empty variable that isn't also in Env", ErrInvalid, k)
		}
		if podmanReads(k) {
			return fmt.Errorf("%w: secret %q has a name podman itself reads, so it would change how podman runs", ErrInvalid, k)
		}
	}
	return nil
}

// podmanReads reports whether podman, or what it runs, reads the
// variable name: a secret, which goes in podman's own environment, must
// never steer it (review 19: CONTAINER_HOST redirected podman).
func podmanReads(k string) bool {
	u := strings.ToUpper(k)
	for _, p := range []string{"CONTAINER", "_CONTAINER", "PODMAN", "BUILDAH", "DOCKER", "REGISTRY", "STORAGE", "XDG_", "LC_", "LANG", "SSL_", "GODEBUG", "GOMAXPROCS", "GOTRACEBACK", "LD_", "DBUS_"} {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	switch u {
	case "PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TZ", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY", "NOTIFY_SOCKET", "TERM":
		return true
	}
	return false
}

// envArgs are the --env flags for s: Env by value, then Secrets by name
// only, each sorted.
func (s Spec) envArgs() []string {
	var argv []string
	for _, k := range sortedKeys(s.Env) {
		argv = append(argv, "--env", k+"="+s.Env[k])
	}
	for _, k := range sortedKeys(s.Secrets) {
		argv = append(argv, "--env", k)
	}
	return argv
}

// secretEnv is podman's environment for s: the host's, cleaned, with the
// secrets' values, which the --env NAME flags read.
func (s Spec) secretEnv() []string {
	env := cleanEnv(os.Environ())
	for _, k := range sortedKeys(s.Secrets) {
		env = append(env, k+"="+s.Secrets[k])
	}
	return env
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Argv is the exact podman command line Run uses for s, without the
// executable and the container's name (NamedArgv). Flags come first and
// are fixed; the image, then s.Args, come last, so nothing in Args can be
// read as a podman flag.
func (s Spec) Argv(timeout time.Duration) []string { return s.NamedArgv("", timeout) }

// NamedArgv is Argv for a container named name: Run names each one, so
// that it can remove one whose step was stopped (BOB-508).
func (s Spec) NamedArgv(name string, timeout time.Duration) []string {
	argv := []string{"run", "--rm", "-i"}
	if name != "" {
		argv = append(argv, "--name", name)
	}
	argv = append(argv,
		"--pull=never",                     // only images already present: no surprise downloads
		"--network=none",                   // no network at all
		"--cap-drop=all",                   // no Linux capabilities
		"--security-opt=no-new-privileges", // setuid binaries can't gain any
		"--pids-limit="+strconv.Itoa(PidsLimit),
		"--memory="+MemoryLimit,
		"--log-driver=none",  // output goes only to us
		"--http-proxy=false", // don't copy the host's proxy variables in
	)
	if timeout > 0 {
		argv = append(argv, "--timeout="+strconv.Itoa(int(math.Ceil(timeout.Seconds()))))
	}
	argv = append(argv, s.Labels.args("container")...)
	argv = append(argv, s.envArgs()...)
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
	s.Labels = s.Labels.orFrom(ctx)
	var timeout time.Duration
	if d, ok := ctx.Deadline(); ok {
		if timeout = time.Until(d); timeout <= 0 {
			return nil, ctx.Err()
		}
	}
	podman := r.podman()
	id := make([]byte, 8)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	name := "bobbin-run-" + hex.EncodeToString(id)
	cmd := exec.CommandContext(ctx, podman, s.NamedArgv(name, timeout)...)
	cmd.Env = s.secretEnv()
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	cmd.Stdin = bytes.NewReader(s.Stdin)
	stdout := &capped{max: MaxOutput, onFull: func() { _ = cmd.Process.Signal(syscall.SIGTERM) }}
	stderr := &tail{max: StderrTail}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	if ctx.Err() != nil || stdout.full {
		// The step was stopped (a run cancelled, a deadline) or wrote too
		// much: podman was told to stop, but a container whose first
		// process ignores SIGTERM would outlive it. Remove it by name, as
		// a session's Close does (BOB-508).
		rm, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		rmCmd := exec.CommandContext(rm, podman, "rm", "--force", "--time", "0", "--ignore", name)
		rmCmd.Env = cleanEnv(os.Environ())
		_ = rmCmd.Run()
		cancel()
	}
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
// ErrInvalid; a base image not on the worker (podman image exists, asked
// before building) wraps ErrSetup; a build that fails is an *ExitError.
func (r Runner) Build(ctx context.Context, b Build) (string, error) {
	bases, err := checkContainerfile(b.Containerfile)
	if err != nil {
		return "", err
	}
	// Builds never pull, so every base must already be here; asking podman
	// first gives an exact answer, rather than reading it from a build log.
	for _, ref := range bases {
		cmd := exec.CommandContext(ctx, r.podman(), "image", "exists", ref)
		cmd.Env = cleanEnv(os.Environ())
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() == 1 {
				return "", fmt.Errorf("%w: the base image %s isn't on the worker (builds never pull)", ErrSetup, ref)
			}
			return "", fmt.Errorf("%w: podman image exists %s: %v", ErrSetup, ref, err)
		}
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
		if !errors.As(err, &exit) {
			return "", fmt.Errorf("%w: %v", ErrSetup, err)
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
