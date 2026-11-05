package bobbin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// A Session is one fresh container that several commands run in, one after
// another: an agent's workspace. It has the same hardening as Run's
// containers (no network, no capabilities, no mounts, limited processes
// and memory, nothing inherited from the host) and is removed by Close.
// Nothing in it is visible to any other step, and nothing survives it
// except what is read out with Read or listed by Diff.
type Session struct {
	podman string
	name   string
}

// Session limits.
const (
	// SessionLabel marks session containers, so they can be found.
	SessionLabel = "bobbin.session"
	// MaxSessionLife bounds how long a session's container may exist,
	// whatever its caller does: Podman stops it then (--timeout), and
	// --rm removes it, so a session whose worker died never outlives it.
	MaxSessionLife = 30 * time.Minute
	// MaxExecOutput is how much of each of a command's standard output and
	// standard error is kept; the rest is dropped and the result says so.
	MaxExecOutput = 64 << 10
	// Workdir is where commands run. Start creates it.
	Workdir = "/work"
)

// keepAlive is the session container's own process: it waits, doing
// nothing, until it's stopped.
const keepAlive = `trap 'exit 0' TERM; while :; do sleep 3600 & wait $!; done`

// SessionArgv is the exact podman command line that starts a session's
// container named name for s. Flags come first and are fixed, and the
// image's entrypoint is replaced by /bin/sh running keepAlive, so the image
// must have a shell. It has the same hardening as Argv, minus -i.
func (s Spec) SessionArgv(name string, life time.Duration) []string {
	argv := []string{
		"run", "--detach", "--rm",
		"--name", name,
		"--label", SessionLabel + "=1",
		"--pull=never",
		"--network=none",
		"--cap-drop=all",
		"--security-opt=no-new-privileges",
		"--pids-limit=" + strconv.Itoa(PidsLimit),
		"--memory=" + MemoryLimit,
		"--log-driver=none",
		"--http-proxy=false",
		"--timeout=" + strconv.Itoa(int(math.Ceil(life.Seconds()))),
		"--entrypoint", "/bin/sh",
	}
	argv = append(argv, s.Labels.args("agent")...)
	argv = append(argv, s.envArgs()...)
	return append(argv, s.Image, "-c", keepAlive)
}

// Start starts a session's container from s (its Args must be empty: the
// session runs commands with Exec instead) and creates Workdir. The session
// lasts at most until ctx's deadline, or MaxSessionLife.
func (r Runner) Start(ctx context.Context, s Spec) (*Session, error) {
	s.Labels = s.Labels.orFrom(ctx) // checked by Validate (BOB-501)
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if len(s.Args) > 0 || len(s.Stdin) > 0 {
		return nil, fmt.Errorf("%w: a session takes no arguments or standard input; run commands with Exec", ErrInvalid)
	}
	life := MaxSessionLife
	if d, ok := ctx.Deadline(); ok && time.Until(d) < life {
		if life = time.Until(d); life <= 0 {
			return nil, ctx.Err()
		}
	}
	id := make([]byte, 8)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	ss := &Session{podman: r.podman(), name: "bobbin-session-" + hex.EncodeToString(id)}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, ss.podman, s.SessionArgv(ss.name, life)...)
	cmd.Env = s.secretEnv()
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		ss.Close() // in case podman created it before failing
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: podman run: %v: %s", ErrSetup, err, strings.TrimSpace(stderr.String()))
	}
	// The first command can't run in Workdir, because it creates it.
	mk := exec.CommandContext(ctx, ss.podman, "exec", ss.name, "/bin/sh", "-c", "mkdir -p "+Workdir)
	mk.Env = cleanEnv(os.Environ())
	if out, err := mk.CombinedOutput(); err != nil {
		ss.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: can't create %s (the image needs /bin/sh, and a %s its user can write or create): %v: %s", ErrSetup, Workdir, Workdir, err, strings.TrimSpace(string(out)))
	}
	return ss, nil
}

func (r Runner) podman() string {
	if r.Podman == "" {
		return "podman"
	}
	return r.Podman
}

// Name is the session container's name.
func (s *Session) Name() string { return s.name }

// ExecResult is what one command did.
type ExecResult struct {
	ExitCode  int    `json:"exitCode"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Truncated bool   `json:"truncated,omitempty"` // an output was longer than MaxExecOutput and was cut
}

// ExecArgv is the exact podman command line that runs command in the
// session, with the given standard input, in Workdir.
func (s *Session) ExecArgv(command string, args ...string) []string {
	return append([]string{"exec", "--interactive", "--workdir", Workdir, s.name, "/bin/sh", "-c", command, "sh"}, args...)
}

// Exec runs command with /bin/sh -c in the session and waits for it. A
// command that runs and exits non-zero is a result, not an error; an error
// means podman couldn't run it (ErrSetup), or ctx ended. When ctx ends,
// podman exec is stopped; anything the command left running inside ends
// with the session.
func (s *Session) Exec(ctx context.Context, command string, stdin []byte, args ...string) (ExecResult, error) {
	cmd := exec.CommandContext(ctx, s.podman, s.ExecArgv(command, args...)...)
	cmd.Env = cleanEnv(os.Environ())
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdin = bytes.NewReader(stdin)
	stdout, stderr, last := &cut{max: MaxExecOutput}, &cut{max: MaxExecOutput}, &tail{max: StderrTail}
	cmd.Stdout, cmd.Stderr = stdout, io.MultiWriter(stderr, last)
	err := cmd.Run()
	res := ExecResult{Stdout: stdout.buf.String(), Stderr: stderr.buf.String(), Truncated: stdout.cut || stderr.cut}
	if err == nil {
		return res, nil
	}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return res, fmt.Errorf("%w: %v", ErrSetup, err)
	}
	res.ExitCode = exit.ExitCode()
	if (res.ExitCode == 125 || res.ExitCode == 126 || res.ExitCode == 127) && podmanError(string(last.buf)) {
		return res, fmt.Errorf("%w: podman exec exited %d: %s", ErrSetup, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return res, nil
}

// Command is a long-running command in the session, for its caller to
// start, stream (Stdin, Stdout) and wait for, such as a server speaking
// over standard input and output. Its standard error is discarded unless
// the caller sets it. Cancelling ctx stops podman exec; what the command
// left running ends with the session.
func (s *Session) Command(ctx context.Context, command string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, s.podman, s.ExecArgv(command, args...)...)
	cmd.Env = cleanEnv(os.Environ())
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// Stop kills every process the session's commands left running (a
// command cut off by its context keeps running inside otherwise), leaving
// the session itself, and its files, as they are.
func (s *Session) Stop(ctx context.Context) error {
	res, err := s.Exec(ctx, "kill -9 -1 2>/dev/null; true", nil)
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("bobbin: stopping the session's commands: %s", strings.TrimSpace(res.Stderr))
	}
	return err
}

// Write puts data at path in the session, creating its directory. The
// path is passed as an argument, never as part of a command.
func (s *Session) Write(ctx context.Context, path string, data []byte) error {
	if !strings.HasPrefix(path, "/") || strings.ContainsRune(path, 0) {
		return fmt.Errorf("%w: path %q must be absolute", ErrInvalid, path)
	}
	res, err := s.Exec(ctx, `mkdir -p "$(dirname "$1")" && cat > "$1"`, data, path)
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("bobbin: writing %s: %s", path, strings.TrimSpace(res.Stderr))
	}
	return err
}

// Read returns the contents of path in the session, at most max bytes:
// ErrOutputTooLarge if it's longer.
func (s *Session) Read(ctx context.Context, path string, max int) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := s.ReadTo(ctx, path, &buf, int64(max)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ReadTo streams the contents of path in the session to w, and returns
// how many bytes it wrote: at most max, or ErrOutputTooLarge if the file
// is longer (w then holds a cut-off prefix, which callers must discard).
// It reads through podman exec, like every other session command, so a
// file of any size leaves the container without touching the host's
// paths.
func (s *Session) ReadTo(ctx context.Context, path string, w io.Writer, max int64) (int64, error) {
	cmd := exec.CommandContext(ctx, s.podman, s.ExecArgv(`cat -- "$1"`, path)...)
	cmd.Env = cleanEnv(os.Environ())
	out := &limited{w: w, max: max, stop: func() { _ = cmd.Process.Signal(syscall.SIGTERM) }}
	stderr := &tail{max: StderrTail}
	cmd.Stdout, cmd.Stderr = out, stderr
	err := cmd.Run()
	switch {
	case out.full:
		return out.n, fmt.Errorf("%w: %s is more than %d bytes", ErrOutputTooLarge, path, max)
	case out.err != nil:
		return out.n, fmt.Errorf("bobbin: reading %s: %w", path, out.err)
	case err != nil && ctx.Err() != nil:
		return out.n, ctx.Err()
	case err != nil:
		return out.n, fmt.Errorf("bobbin: reading %s: %v: %s", path, err, strings.TrimSpace(string(stderr.buf)))
	}
	return out.n, nil
}

// limited passes at most max bytes to w; past that, or when w fails, it
// stops the writer and drops the rest.
type limited struct {
	w    io.Writer
	max  int64
	n    int64
	full bool
	err  error
	stop func()
}

func (l *limited) Write(p []byte) (int, error) {
	if l.full || l.err != nil {
		return len(p), nil
	}
	if l.n+int64(len(p)) > l.max {
		l.full = true
		l.stop()
		return len(p), nil
	}
	n, err := l.w.Write(p)
	l.n += int64(n)
	if err != nil {
		l.err = err
		l.stop()
	}
	return len(p), nil
}

// Change is one path the session added, changed or deleted, relative to
// its image.
type Change struct {
	Kind string `json:"kind"` // "added", "changed" or "deleted"
	Path string `json:"path"`
}

// Diff lists what the session changed in its container's writable layer,
// relative to its image (podman diff), sorted by path.
func (s *Session) Diff(ctx context.Context) ([]Change, error) {
	cmd := exec.CommandContext(ctx, s.podman, "diff", "--format", "json", s.name)
	cmd.Env = cleanEnv(os.Environ())
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("bobbin: podman diff: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	var d map[string][]string
	if err := json.Unmarshal(stdout.Bytes(), &d); err != nil {
		return nil, fmt.Errorf("bobbin: podman diff: %v", err)
	}
	var out []Change
	for _, kind := range []string{"added", "changed", "deleted"} {
		for _, p := range d[kind] {
			out = append(out, Change{Kind: kind, Path: p})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Close removes the session's container and everything in it. It's safe
// to call more than once, and works even after ctx has ended.
func (s *Session) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.podman, "rm", "--force", "--time", "0", "--ignore", s.name)
	cmd.Env = cleanEnv(os.Environ())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("bobbin: removing session %s: %v: %s", s.name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// cut keeps the first max bytes and notes that more were dropped, without
// stopping the writer.
type cut struct {
	buf bytes.Buffer
	max int
	cut bool
}

func (c *cut) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); len(p) > room {
		c.buf.Write(p[:max(room, 0)])
		c.cut = true
		return len(p), nil
	}
	return c.buf.Write(p)
}
