package bobbin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/temporal"

	"github.com/heedle-ai/bobbin"
	"github.com/heedle-ai/shuttle/plan"
)

var pin = "docker.io/library/busybox@sha256:" + zeros

// TestInv_ContainerfilesAreChecked: the image command builds only
// Containerfiles it can read with certainty, whose every base is pinned to
// content, and which can't reach a network or the host: no ADD, ONBUILD,
// RUN flags, unpinned FROM or COPY --from, parser directives or heredocs.
func TestInv_ContainerfilesAreChecked(t *testing.T) {
	for _, ok := range []string{
		"FROM " + pin + "\nRUN echo hi > /a\nCOPY in/n0.txt /n0.txt\nCMD [\"cat\", \"/a\"]\n",
		"ARG X=1\nFROM " + zeros + " AS build\nRUN make \\\n  all\nFROM scratch\nCOPY --from=build --chown=1:1 /out /out\n",
		"# a comment\n\nFROM sha256:" + zeros + "\nCOPY --from=" + pin + " /bin/sh /sh\n",
	} {
		if err := bobbin.CheckContainerfile(ok); err != nil {
			t.Errorf("refused %q: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"", "RUN echo\n", "FROM busybox\n", "FROM busybox:1.36\n", "FROM ${BASE}\n", "ARG B\nFROM $B\n",
		"FROM --platform=linux/arm64 " + pin + "\n", "FROM " + pin + " AS\n", "FROM " + pin + " junk words\n",
		"FROM " + pin + "\nADD https://example.com/x /x\n", "FROM " + pin + "\nADD in/n0.txt /x\n",
		"FROM " + pin + "\nONBUILD RUN curl evil\n", "FROM " + pin + "\nRUN --network=host curl x\n",
		"FROM " + pin + "\nRUN --mount=type=bind,source=/,target=/h ls\n", "FROM " + pin + "\nCOPY --from=busybox /bin /bin\n",
		"FROM " + pin + "\nCOPY --parents in /x\n", "# syntax=docker/dockerfile:1\nFROM " + pin + "\n",
		"# escape=`\nFROM " + pin + "\n", "FROM " + pin + "\nRUN <<EOF\ncurl x\nEOF\n",
		"FROM " + pin + "\nRUN echo \\\n --network\nFROM busybox\n", "FROM " + pin + "\x00\n",
		"FROM " + pin + "\nCOPY --from=ghost /x /x\n", strings.Repeat("#", bobbin.MaxContainerfile) + "\nFROM " + pin,
	} {
		if err := bobbin.CheckContainerfile(bad); !errors.Is(err, bobbin.ErrInvalid) {
			t.Errorf("accepted %q", bad)
		}
	}
}

// TestInv_ExactBuildCommandLines: builds and pushes run exactly these
// podman command lines, and a build's context holds only its
// Containerfile and in/<node>.txt for each input.
func TestInv_ExactBuildCommandLines(t *testing.T) {
	want := []string{"build", "--pull=never", "--network=none", "--security-opt=no-new-privileges",
		"--memory=2g", "--http-proxy=false", "--timestamp=0", "--identity-label=false", "--iidfile", "/t/iid",
		"--file", "/t/context/Containerfile", "/t/context"}
	if got := bobbin.BuildArgv("/t/context", "/t/iid"); !reflect.DeepEqual(got, want) {
		t.Errorf("build argv %q", got)
	}
	id := "sha256:" + zeros
	if got := bobbin.PushArgv(id, "localhost:5000/acme/images", "/t/d", true); !reflect.DeepEqual(got, []string{"push", "--format", "oci",
		"--digestfile", "/t/d", "--tls-verify=false", id, "docker://localhost:5000/acme/images:sha256-" + zeros}) {
		t.Errorf("push argv %q", got)
	}

	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	podman := filepath.Join(dir, "podman")
	// The fake lists the build context, then writes the image ID.
	os.WriteFile(podman, []byte(`#!/bin/sh
printf '%s\n' "$*" >> `+log+`
ctx=""; iid=""; prev=""
for a in "$@"; do [ "$prev" = --iidfile ] && iid=$a; prev=$a; ctx=$a; done
(cd "$ctx" && find . | sort) >> `+log+`
echo `+id+` > "$iid"
`), 0o755)
	got, err := bobbin.Runner{Podman: podman}.Build(context.Background(), bobbin.Build{Containerfile: "FROM " + pin + "\n",
		Inputs: map[string]string{"n1": "b", "n0": "a"}})
	if err != nil || got != id {
		t.Fatalf("build = %q, %v", got, err)
	}
	b, _ := os.ReadFile(log)
	if lines := strings.Split(strings.TrimSpace(string(b)), "\n"); strings.Join(lines[1:], " ") != ". ./Containerfile ./in ./in/n0.txt ./in/n1.txt" {
		t.Errorf("context: %q", lines[1:])
	}
	for _, bad := range []map[string]string{{"../x": "a"}, {"n0/x": "a"}, {"": "a"}} {
		if _, err := (bobbin.Runner{Podman: podman}).Build(context.Background(), bobbin.Build{Containerfile: "FROM " + pin, Inputs: bad}); !errors.Is(err, bobbin.ErrInvalid) {
			t.Errorf("input names %v: %v", bad, err)
		}
	}
}

// TestInv_ImageCommand: the image command takes a Containerfile and an
// optional "push"; a push with no registry fails with ImageSetup before
// building; a refused Containerfile fails with InvalidInput; the output
// is the image ID as JSON.
func TestInv_ImageCommand(t *testing.T) {
	dir := t.TempDir()
	podman := filepath.Join(dir, "podman")
	os.WriteFile(podman, []byte("#!/bin/sh\necho build >> "+filepath.Join(dir, "log")+"\nfor a in \"$@\"; do [ \"$p\" = --iidfile ] && echo sha256:"+zeros+" > \"$a\"; p=$a; done\n"), 0o755)
	cmd := bobbin.ImageCommand(bobbin.Runner{Podman: podman}, nil)
	kind := func(err error) string {
		var app *temporal.ApplicationError
		if errors.As(err, &app) {
			return app.Type()
		}
		return fmt.Sprint(err)
	}
	for args, want := range map[string]string{"": "InvalidInput", "FROM " + pin + "|publish": "InvalidInput", "FROM busybox": "InvalidInput",
		"FROM " + pin + "|push": "ImageSetup", "FROM " + pin + "|push|x": "InvalidInput"} {
		var a []string
		if args != "" {
			a = strings.Split(args, "|")
		}
		if _, err := cmd(context.Background(), plan.Input{Args: a}); kind(err) != want {
			t.Errorf("args %q: %v, want %s", a, err, want)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "log")); len(b) != 0 {
		t.Errorf("podman ran for refused commands: %s", b)
	}
	out, err := cmd(context.Background(), plan.Input{Args: []string{"FROM " + pin}})
	if err != nil || out != `{"image":"sha256:`+zeros+`"}` {
		t.Errorf("out %s, %v", out, err)
	}
}

// TestContainer_ImageBuildIsSandboxed: a real build runs its RUN steps
// with no network, sees its inputs in in/, gives the same image ID when
// built again, and refuses a base image the worker doesn't have.
func TestContainer_ImageBuildIsSandboxed(t *testing.T) {
	img := strings.TrimPrefix(shellImage(t), "sha256:")
	r := bobbin.Runner{}
	b := bobbin.Build{Containerfile: "FROM " + img + "\nRUN ls /sys/class/net > /nets\nCOPY in/n0.txt /n0.txt\n",
		Inputs: map[string]string{"n0": "from n0\n"}}
	id, err := r.Build(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Run(ctx, bobbin.Spec{Image: id, Args: []string{"/bin/sh", "-c", "cat /nets /n0.txt"}})
	if err != nil || string(out) != "lo\nfrom n0\n" {
		t.Errorf("built image says %q, %v", out, err)
	}
	if again, err := r.Build(ctx, b); err != nil || again != id {
		t.Errorf("rebuilt as %s, %v; want %s", again, err, id)
	}
	if _, err := r.Build(ctx, bobbin.Build{Containerfile: "FROM " + strings.Repeat("ab", 32) + "\n"}); !errors.Is(err, bobbin.ErrSetup) {
		t.Errorf("missing base: %v", err)
	}
}

// TestContainer_ImagePushes: a pushed image is in the registry under the
// digest Push reports.
func TestContainer_ImagePushes(t *testing.T) {
	img := strings.TrimPrefix(shellImage(t), "sha256:")
	reg, err := exec.LookPath("registry")
	if err != nil {
		if os.Getenv("BOBBIN_REQUIRE_PODMAN") == "1" {
			t.Fatal("the distribution registry isn't installed")
		}
		t.Skip("the distribution registry isn't installed")
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yml")
	os.WriteFile(cfg, []byte(fmt.Sprintf("version: 0.1\nstorage:\n  filesystem:\n    rootdirectory: %s\nhttp:\n  addr: %s\n", filepath.Join(dir, "data"), addr)), 0o644)
	srv := exec.Command(reg, "serve", cfg)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { srv.Process.Kill(); srv.Wait() }()
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	out, err := bobbin.ImageCommand(bobbin.Runner{}, &bobbin.ImageRegistry{Repo: addr + "/heedle/images", PlainHTTP: true})(ctx,
		plan.Input{Args: []string{"FROM " + img + "\nRUN echo pushed > /p\n", "push"}})
	if err != nil {
		t.Fatal(err)
	}
	var res bobbin.ImageResult
	json.Unmarshal([]byte(out), &res)
	repo, digest, ok := strings.Cut(res.Pushed, "@")
	if !ok || repo != addr+"/heedle/images" {
		t.Fatalf("pushed %q", res.Pushed)
	}
	req, _ := http.NewRequest("HEAD", "http://"+addr+"/v2/heedle/images/manifests/"+digest, nil)
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Errorf("the registry doesn't have %s: %v %v", res.Pushed, resp, err)
	}
}
