package bobbin_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/heedle-ai/bobbin"
)

var shellImageOnce = sync.OnceValues(func() (string, error) {
	bb, err := exec.LookPath("busybox")
	if err != nil {
		return "", errors.New("needs a static busybox on PATH (Debian, Ubuntu: apt-get install busybox-static)")
	}
	dir, err := os.MkdirTemp("", "bobbin-shell")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	b, err := os.ReadFile(bb)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "busybox"), b, 0o755); err != nil {
		return "", err
	}
	cf := "FROM scratch\nCOPY busybox /bin/busybox\nRUN [\"/bin/busybox\", \"--install\", \"-s\", \"/bin\"]\nRUN [\"/bin/mkdir\", \"-p\", \"/work\", \"/tmp\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "Containerfile"), []byte(cf), 0o644); err != nil {
		return "", err
	}
	out, err := exec.Command("podman", "build", "--quiet", dir).Output()
	return strings.TrimSpace(string(out)), err
})

// shellImage is a FROM scratch image with a static busybox: a shell and
// the usual tools, for session tests.
func shellImage(t *testing.T) string {
	t.Helper()
	buildImage(t, "./testdata/agent", "agent") // skips or fails, as every container test does, without podman
	id, err := shellImageOnce()
	if err != nil {
		if os.Getenv("BOBBIN_REQUIRE_PODMAN") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	return id
}

func exists(name string) bool {
	return exec.Command("podman", "container", "exists", name).Run() == nil
}

// TestContainer_SessionIsOneFreshWorkspace: a session's commands share one
// container: what one writes, the next reads; what changed is listed
// against the image; it has no network; another session sees none of it;
// and Close removes it.
func TestContainer_SessionIsOneFreshWorkspace(t *testing.T) {
	img := shellImage(t)
	r := bobbin.Runner{}
	a, err := r.Start(ctx, bobbin.Spec{Image: img, Env: map[string]string{"HEEDLE_NODE": "n3"}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.Write(ctx, "/work/in/n0.txt", []byte("from n0\n")); err != nil {
		t.Fatal(err)
	}
	res, err := a.Exec(ctx, `cat in/n0.txt > report.txt && echo "$HEEDLE_NODE" >> report.txt && pwd`, nil)
	if err != nil || res.ExitCode != 0 || res.Stdout != "/work\n" {
		t.Fatalf("exec = %+v, %v", res, err)
	}
	if b, err := a.Read(ctx, "/work/report.txt", 1000); err != nil || string(b) != "from n0\nn3\n" {
		t.Errorf("read = %q, %v", b, err)
	}
	changes, err := a.Diff(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range changes {
		got[c.Path] = c.Kind
	}
	if got["/work/report.txt"] != "added" || got["/work/in/n0.txt"] != "added" {
		t.Errorf("diff = %+v", changes)
	}
	if res, err := a.Exec(ctx, "wget -q -T 3 -O /dev/null http://1.1.1.1/", nil); err != nil || res.ExitCode == 0 {
		t.Errorf("a session reached the network: %+v, %v", res, err)
	}
	if res, err := a.Exec(ctx, "exit 3", nil); err != nil || res.ExitCode != 3 {
		t.Errorf("a failing command is a result: %+v, %v", res, err)
	}
	b, err := r.Start(ctx, bobbin.Spec{Image: img})
	if err != nil {
		t.Fatal(err)
	}
	if res, _ := b.Exec(ctx, "test -e /work/report.txt", nil); res.ExitCode == 0 {
		t.Error("another session sees this one's files")
	}
	b.Close()
	a.Close()
	if exists(a.Name()) || exists(b.Name()) {
		t.Error("Close left a container")
	}
}

// TestContainer_CancelledExecEndsWithTheSession: a command cut off
// by its context returns the context's error, and the session can still
// be closed.
func TestContainer_CancelledExecEndsWithTheSession(t *testing.T) {
	img := shellImage(t)
	s, err := bobbin.Runner{}.Start(ctx, bobbin.Spec{Image: img})
	if err != nil {
		t.Fatal(err)
	}
	cctx, cancel := contextWithTimeout(2)
	defer cancel()
	if _, err := s.Exec(cctx, "sleep 60", nil); err == nil {
		t.Error("a command past its deadline returned no error")
	}
	if err := s.Close(); err != nil || exists(s.Name()) {
		t.Errorf("close after a cancelled command: %v", err)
	}
}

func contextWithTimeout(seconds int) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
}

// TestContainer_StopEndsLeftoverCommands: a command cut off by its context
// keeps running inside the session; Stop kills it, so it can't change the
// session's files afterwards.
func TestContainer_StopEndsLeftoverCommands(t *testing.T) {
	img := shellImage(t)
	s, err := bobbin.Runner{}.Start(ctx, bobbin.Spec{Image: img})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cctx, cancel := contextWithTimeout(1)
	s.Exec(cctx, "sleep 3; echo late > /work/late", nil)
	cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(4 * time.Second)
	if res, _ := s.Exec(ctx, "test -e /work/late", nil); res.ExitCode == 0 {
		t.Error("a stopped command still wrote its file")
	}
}

// TestContainer_SessionEndsAtItsDeadline: a session nobody closes (its
// worker died) is stopped and removed by Podman at its deadline.
func TestContainer_SessionEndsAtItsDeadline(t *testing.T) {
	img := shellImage(t)
	dctx, cancel := contextWithTimeout(3)
	defer cancel()
	s, err := bobbin.Runner{}.Start(dctx, bobbin.Spec{Image: img})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20 && exists(s.Name()); i++ {
		time.Sleep(time.Second)
	}
	if exists(s.Name()) {
		s.Close()
		t.Error("a session outlived its deadline")
	}
}

type failingWriter struct{ after int }

func (f *failingWriter) Write(p []byte) (int, error) {
	if f.after -= len(p); f.after < 0 {
		return 0, errors.New("disk full")
	}
	return len(p), nil
}

// TestContainer_ReadToStreamsLargeFiles: ReadTo copies a file far larger
// than Read's limits out of the session byte for byte, refuses one longer
// than max with ErrOutputTooLarge, and reports a writer that fails.
func TestContainer_ReadToStreamsLargeFiles(t *testing.T) {
	img := shellImage(t)
	s, err := bobbin.Runner{}.Start(ctx, bobbin.Spec{Image: img})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if res, err := s.Exec(ctx, `head -c 3000000 /dev/urandom > big.bin`, nil); err != nil || res.ExitCode != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	h := sha256.New()
	n, err := s.ReadTo(ctx, "/work/big.bin", h, 3000000)
	sum, _ := s.Exec(ctx, `sha256sum big.bin`, nil)
	if err != nil || n != 3000000 || !strings.HasPrefix(sum.Stdout, hex.EncodeToString(h.Sum(nil))) {
		t.Errorf("ReadTo: %d bytes, %v; container says %s", n, err, sum.Stdout)
	}
	if _, err := s.ReadTo(ctx, "/work/big.bin", io.Discard, 2999999); !errors.Is(err, bobbin.ErrOutputTooLarge) {
		t.Errorf("one byte too long: %v", err)
	}
	if _, err := s.ReadTo(ctx, "/work/big.bin", &failingWriter{after: 100000}, 3000000); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("failing writer: %v", err)
	}
}
