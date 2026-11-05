package bobbin_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"

	"github.com/heedle-ai/bobbin"
	"github.com/heedle-ai/shuttle"
	"github.com/heedle-ai/shuttle/plan"
)

// argsOf is the arguments a fake podman that prints one per line got.
func argsOf(out []byte) []string {
	return strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
}

// labelsIn are the --label values in argv, in order, and whether every one
// comes after the fixed flags and before the image.
func labelsIn(argv []string, image string) (labels []string, placed bool) {
	img := slices.Index(argv, image)
	placed = img > 0
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "--label" && strings.HasPrefix(argv[i+1], "heedle.") {
			labels = append(labels, argv[i+1])
			if i > img || slices.Contains(argv[:i], "--env") {
				placed = false
			}
		}
	}
	return labels, placed
}

// TestInv_EveryContainerIsLabelled: every container bobbin starts carries
// heedle.kind (container for a one-shot run, agent for a session, or the
// kind given), and the run labels given or carried by the context, as
// --label flags after the fixed flags and before the environment and the
// image (BOB-500).
func TestInv_EveryContainerIsLabelled(t *testing.T) {
	podman := fakePodman(t, `for a in "$@"; do printf '%s\n' "$a"; done`)
	r := bobbin.Runner{Podman: podman}
	run := bobbin.Labels{Workflow: "loom-abc/plan", Run: "6f1c2a7d-3b8e", Step: "n2"}
	cases := []struct {
		name string
		ctx  context.Context
		l    bobbin.Labels
		want []string
	}{
		{"one-shot, nothing given", ctx, bobbin.Labels{}, []string{"heedle.kind=container"}},
		{"one-shot, a run in the context", bobbin.WithLabels(ctx, run), bobbin.Labels{}, []string{"heedle.kind=container", "heedle.workflow=loom-abc/plan", "heedle.run=6f1c2a7d-3b8e", "heedle.step=n2"}},
		{"one-shot, given its own", bobbin.WithLabels(ctx, run), bobbin.Labels{Kind: "connector", Workflow: "w", Run: "r"}, []string{"heedle.kind=connector", "heedle.workflow=w", "heedle.run=r"}},
	}
	for _, c := range cases {
		out, err := r.Run(c.ctx, bobbin.Spec{Image: zeros, Env: map[string]string{"A": "1"}, Labels: c.l})
		if err != nil {
			t.Fatal(c.name, err)
		}
		got, placed := labelsIn(argsOf(out), zeros)
		if !reflect.DeepEqual(got, c.want) || !placed {
			t.Errorf("%s: labels %q (placed after the fixed flags: %v), want %q", c.name, got, placed, c.want)
		}
	}
	// Sessions: the start's arguments, as the fake podman logs them.
	log := filepath.Join(t.TempDir(), "log")
	podman = fakePodman(t, `[ "$1" = run ] && for a in "$@"; do printf '%s\n' "$a"; done >> `+log+`; exit 0`)
	for _, c := range []struct {
		ctx  context.Context
		l    bobbin.Labels
		want []string
	}{
		{ctx, bobbin.Labels{}, []string{"heedle.kind=agent"}},
		{bobbin.WithLabels(ctx, run), bobbin.Labels{Kind: "skill"}, []string{"heedle.kind=skill", "heedle.workflow=loom-abc/plan", "heedle.run=6f1c2a7d-3b8e", "heedle.step=n2"}},
	} {
		os.Remove(log)
		ss, err := bobbin.Runner{Podman: podman}.Start(c.ctx, bobbin.Spec{Image: zeros, Labels: c.l})
		if err != nil {
			t.Fatal(err)
		}
		ss.Close()
		b, _ := os.ReadFile(log)
		got, placed := labelsIn(argsOf(b), zeros)
		if !reflect.DeepEqual(got, c.want) || !placed {
			t.Errorf("session: labels %q (placed: %v), want %q", got, placed, c.want)
		}
		// The old label stays, for anything that looks for it.
		if !strings.Contains(string(b), "bobbin.session=1") {
			t.Error("a session lost its bobbin.session label")
		}
	}
}

// TestInv_LabelsCantInjectFlags: a label podman would read as anything
// but a label is refused, as ErrInvalid, before podman runs (BOB-501).
func TestInv_LabelsCantInjectFlags(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	podman := fakePodman(t, "touch "+marker)
	for _, l := range []bobbin.Labels{
		{Kind: "privileged"}, {Kind: "agent --privileged"}, {Step: "--privileged"}, {Step: "-v"},
		{Workflow: "w --volume=/:/host"}, {Run: "a=b"}, {Step: "n0\n--privileged"}, {Step: "n,0"},
		{Workflow: strings.Repeat("w", 256)}, {Run: "r\x00"}, {Step: "ünï"}, {Step: `"quoted"`},
	} {
		_, err := bobbin.Runner{Podman: podman}.Run(ctx, bobbin.Spec{Image: zeros, Labels: l})
		if !errors.Is(err, bobbin.ErrInvalid) {
			t.Errorf("Run with labels %+v: %v, want ErrInvalid", l, err)
		}
		if _, err := (bobbin.Runner{Podman: podman}).Start(ctx, bobbin.Spec{Image: zeros, Labels: l}); !errors.Is(err, bobbin.ErrInvalid) {
			t.Errorf("Start with labels %+v: %v, want ErrInvalid", l, err)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("podman ran for labels that should have been refused")
	}
	// What StepLabels reads from outside is never refused: a value a label
	// can't hold is left out, so labelling never fails a step.
	if l := bobbin.StepLabels(ctx, "--privileged"); l != (bobbin.Labels{}) {
		t.Errorf("StepLabels kept a bad step: %+v", l)
	}
}

// FuzzLabels: whatever the labels, either they're refused or each is one
// "--label" "heedle.NAME=VALUE" pair between the fixed flags and the
// image, and nothing else changes on the command line (BOB-501).
func FuzzLabels(f *testing.F) {
	for _, s := range [][4]string{{"agent", "loom-abc/plan", "6f1c2a7d", "n0"}, {"", "", "", "--privileged"},
		{"container", "w --volume=/:/h", "r", "s"}, {"act", "a=b", "-v", "x\ny"}} {
		f.Add(s[0], s[1], s[2], s[3])
	}
	var base []string
	for _, a := range (bobbin.Spec{Image: zeros, Args: []string{"x"}}).Argv(0) {
		if a != "--label" && !strings.HasPrefix(a, "heedle.") {
			base = append(base, a)
		}
	}
	f.Fuzz(func(t *testing.T, kind, wf, run, step string) {
		s := bobbin.Spec{Image: zeros, Args: []string{"x"}, Labels: bobbin.Labels{Kind: kind, Workflow: wf, Run: run, Step: step}}
		if s.Validate() != nil {
			return
		}
		argv := s.Argv(0)
		var rest []string
		for i := 0; i < len(argv); i++ {
			if argv[i] == "--label" && i+1 < len(argv) {
				kv := argv[i+1]
				name, v, _ := strings.Cut(kv, "=")
				if !strings.HasPrefix(name, "heedle.") || !bobbin.ValidLabelValue(v) {
					t.Fatalf("label argument %q", kv)
				}
				i++
				continue
			}
			rest = append(rest, argv[i])
		}
		if !reflect.DeepEqual(rest, base) {
			t.Fatalf("labels changed the command line:\n got  %q\n want %q", rest, base)
		}
	})
}

// TestInv_StepLabelsComeFromTemporal: a step's run labels are what
// Temporal's activity info says (the workflow that scheduled it and its
// run) with the step's node ID; and the container command labels its
// container so, as a "container" (BOB-502).
func TestInv_StepLabelsComeFromTemporal(t *testing.T) {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestActivityEnvironment()
	read := func(ctx context.Context) (bobbin.Labels, error) {
		e := activity.GetInfo(ctx).WorkflowExecution
		l := bobbin.StepLabels(ctx, "n3")
		if l.Workflow != e.ID || l.Run != e.RunID || l.Workflow == "" || l.Run == "" {
			return l, fmt.Errorf("labels %+v, activity info %+v", l, e)
		}
		return l, nil
	}
	env.RegisterActivityWithOptions(read, activity.RegisterOptions{Name: "Read"})
	v, err := env.ExecuteActivity("Read")
	if err != nil {
		t.Fatal(err)
	}
	var l bobbin.Labels
	if err := v.Get(&l); err != nil || l.Step != "n3" {
		t.Fatalf("StepLabels = %+v, %v", l, err)
	}
	// Outside an activity there's no run: only the step.
	if got := bobbin.StepLabels(ctx, "n3"); got != (bobbin.Labels{Step: "n3"}) {
		t.Errorf("outside an activity, StepLabels = %+v", got)
	}

	podman := fakePodman(t, `cat >/dev/null; for a in "$@"; do printf '%s\n' "$a"; done`)
	env = s.NewTestActivityEnvironment()
	env.RegisterActivityWithOptions(shuttle.ArrowAsActivity(bobbin.Command(bobbin.Runner{Podman: podman})), activity.RegisterOptions{Name: bobbin.ActivityName})
	v, err = env.ExecuteActivity(bobbin.ActivityName, plan.Input{NodeID: "n7", Args: []string{zeros}})
	if err != nil {
		t.Fatal(err)
	}
	var out string
	if err := v.Get(&out); err != nil {
		t.Fatal(err)
	}
	got, placed := labelsIn(argsOf([]byte(out)), zeros)
	want := []string{"heedle.kind=container", "heedle.workflow=" + l.Workflow, "heedle.run=" + l.Run, "heedle.step=n7"}
	if !reflect.DeepEqual(got, want) || !placed {
		t.Errorf("container step labels %q (placed: %v), want %q", got, placed, want)
	}
}

// TestInv_ContainerStepsHeartbeat: a container step heartbeats while its
// container runs, which is how Temporal tells it the run was cancelled
// (BOB-504).
func TestInv_ContainerStepsHeartbeat(t *testing.T) {
	defer bobbin.SetHeartbeatEvery(20 * time.Millisecond)()
	podman := fakePodman(t, `cat >/dev/null; sleep 0.3; echo done`)
	var s testsuite.WorkflowTestSuite
	env := s.NewTestActivityEnvironment()
	var beats atomic.Int32
	env.SetOnActivityHeartbeatListener(func(*activity.Info, converter.EncodedValues) { beats.Add(1) })
	env.RegisterActivityWithOptions(shuttle.ArrowAsActivity(bobbin.Command(bobbin.Runner{Podman: podman})), activity.RegisterOptions{Name: bobbin.ActivityName})
	if _, err := env.ExecuteActivity(bobbin.ActivityName, plan.Input{NodeID: "n0", Args: []string{zeros}}); err != nil {
		t.Fatal(err)
	}
	// Temporal's SDK throttles heartbeats, so a short step sends one.
	if beats.Load() < 1 {
		t.Errorf("a container step heartbeat %d times in 0.3 s", beats.Load())
	}
}

// The ps answer below: one of heedle's containers, one with no labels and
// one with a kind heedle doesn't have, as if podman's filter let them
// through, and one of heedle's with labels a label can't hold.
const (
	heedleID = "aaaa000000000000000000000000000000000000000000000000000000000001"
	mineID   = "bbbb000000000000000000000000000000000000000000000000000000000002"
	evilID   = "cccc000000000000000000000000000000000000000000000000000000000003"
	oddID    = "dddd000000000000000000000000000000000000000000000000000000000004"
	psJSON   = `[{"Id":"` + heedleID + `","Names":["bobbin-session-1"],"Image":"ghcr.io/heedle-ai/pirn-agent@sha256:x","State":"running","Created":1793876670,
	  "Labels":{"heedle.kind":"agent","heedle.workflow":"loom-abc/plan","heedle.run":"r1","heedle.step":"n0","bobbin.session":"1"}},
	 {"Id":"` + mineID + `","Names":["mine"],"Image":"localhost/mine:1","State":"running","Created":1793876600,"Labels":null},
	 {"Id":"` + evilID + `","Names":["evil"],"Image":"x","State":"running","Created":1793876600,"Labels":{"heedle.kind":"evil"}},
	 {"Id":"` + oddID + `","Names":["odd"],"Image":"y","State":"exited","Created":1793876500,"Labels":{"heedle.kind":"browse","heedle.step":"--privileged"}}]`
	statsJSON = `[{"id":"aaaa00000000","name":"bobbin-session-1","cpu_percent":"14.65%","mem_usage":"94.21kB / 536.9MB"}]`
)

// TestInv_OnlyHeedlesContainersAreListedOrRemoved: Containers lists only
// containers with heedle.kind of a kind heedle has, with podman stats'
// figures and only valid labels; Remove removes only what that listing
// found, whatever it's asked (BOB-503).
func TestInv_OnlyHeedlesContainersAreListedOrRemoved(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log")
	podman := fakePodman(t, `echo "$@" >> `+log+`
case "$1" in ps) cat <<'EOF'
`+psJSON+`
EOF
;; stats) echo '`+statsJSON+`';; esac`)
	r := bobbin.Runner{Podman: podman}
	cs, err := r.Containers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[0].ID != oddID || cs[1].ID != heedleID {
		t.Fatalf("Containers = %+v", cs)
	}
	if c := cs[1]; c.Labels != (bobbin.Labels{Kind: "agent", Workflow: "loom-abc/plan", Run: "r1", Step: "n0"}) || c.CPU != "14.65%" || c.Memory != "94.21kB / 536.9MB" || c.Name != "bobbin-session-1" {
		t.Errorf("heedle's container = %+v", c)
	}
	if cs[0].Labels.Step != "" {
		t.Errorf("a label a label can't hold was kept: %+v", cs[0].Labels)
	}
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "ps --all --filter label=heedle.kind --format json") || !strings.Contains(string(b), "stats --no-stream --format json "+heedleID) {
		t.Errorf("podman calls:\n%s", b)
	}
	// Asked to remove everything, it removes only heedle's.
	os.Remove(log)
	gone, err := r.Remove(ctx, func(bobbin.Container) bool { return true })
	if err != nil || len(gone) != 2 {
		t.Fatalf("Remove = %+v, %v", gone, err)
	}
	b, _ = os.ReadFile(log)
	rm := strings.Split(strings.TrimSpace(string(b)), "\n")
	if want := "rm --force --time 0 --ignore " + oddID + " " + heedleID; rm[len(rm)-1] != want {
		t.Errorf("rm = %q, want %q", rm[len(rm)-1], want)
	}
	if strings.Contains(string(b), mineID) || strings.Contains(string(b), evilID) {
		t.Errorf("podman was asked about a container that isn't heedle's:\n%s", b)
	}
}

// TestInv_MachineIsReadFromPodman: the machine is the running one, else
// the default, with memory and disk in bytes as podman 4.9 and 5 write
// them (strings); none when podman lists none (BOB-506).
func TestInv_MachineIsReadFromPodman(t *testing.T) {
	podman := fakePodman(t, `cat <<'EOF'
[{"Name":"other","Default":false,"Running":false,"CPUs":2,"Memory":"2147483648","DiskSize":"10737418240"},
 {"Name":"podman-machine-default","Default":true,"Running":true,"Starting":false,"CPUs":4,"Memory":"4294967296","DiskSize":"107374182400","VMType":"applehv"}]
EOF`)
	m, err := bobbin.Runner{Podman: podman}.Machine(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := (bobbin.Machine{Name: "podman-machine-default", Running: true, CPUs: 4, Memory: 4 << 30, Disk: 100 << 30}); m == nil || *m != want {
		t.Errorf("Machine = %+v, want %+v", m, want)
	}
	podman = fakePodman(t, `echo '[]'`)
	if m, err := (bobbin.Runner{Podman: podman}).Machine(ctx); m != nil || err != nil {
		t.Errorf("no machine: %+v, %v", m, err)
	}
}

// TestInv_PruneRemovesOnlyOldHeedleImages: a prune removes, by name and
// never forced, only names under heedle's repository, of images that
// aren't kept and that no container uses; it needs something to keep and
// a repository that is a registry and an organization (BOB-505).
func TestInv_PruneRemovesOnlyOldHeedleImages(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log")
	podman := fakePodman(t, `echo "$@" >> `+log+`
case "$1" in images) cat <<'EOF'
[{"Id":"1","Names":["ghcr.io/heedle-ai/pirn-agent@sha256:new"],"RepoDigests":["ghcr.io/heedle-ai/pirn-agent@sha256:new"],"Size":10,"Containers":0},
 {"Id":"2","Names":["ghcr.io/heedle-ai/pirn-agent@sha256:old"],"RepoDigests":["ghcr.io/heedle-ai/pirn-agent@sha256:old"],"Size":20,"Containers":0},
 {"Id":"3","Names":["ghcr.io/heedle-ai/reed-browser@sha256:inuse"],"Size":30,"Containers":1},
 {"Id":"4","Names":["localhost/mine:1"],"Size":40,"Containers":0},
 {"Id":"5","Names":["ghcr.io/heedle-ai/reed-browser@sha256:older","localhost/mine:2"],"Size":50,"Containers":0},
 {"Id":"5","Names":["ghcr.io/heedle-ai/reed-browser@sha256:older","localhost/mine:2"],"Size":50,"Containers":0},
 {"Id":"6","Names":["ghcr.io/heedle-ai-evil/x:1","ghcr.io/other/heedle-ai/y:1"],"Size":60,"Containers":0},
 {"Id":"7","Names":["ghcr.io/heedle-ai/reed-browser:dev"],"RepoDigests":["ghcr.io/heedle-ai/reed-browser@sha256:pinned"],"Size":70,"Containers":0}]
EOF
;; esac`)
	r := bobbin.Runner{Podman: podman}
	keep := []string{"ghcr.io/heedle-ai/pirn-agent@sha256:new", "ghcr.io/heedle-ai/reed-browser@sha256:pinned"}
	ims, err := r.Images(ctx, bobbin.HeedleRepo, keep)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, im := range ims {
		ids = append(ids, fmt.Sprintf("%s:%v", im.ID, im.Pinned))
	}
	slices.Sort(ids)
	if want := []string{"1:true", "2:false", "3:false", "5:false", "7:true"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("Images = %v, want %v", ids, want)
	}
	os.Remove(log)
	removed, err := r.PruneImages(ctx, bobbin.HeedleRepo, keep)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ghcr.io/heedle-ai/pirn-agent@sha256:old", "ghcr.io/heedle-ai/reed-browser@sha256:older"}; !reflect.DeepEqual(removed, want) {
		t.Errorf("removed %v, want %v", removed, want)
	}
	b, _ := os.ReadFile(log)
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.HasPrefix(l, "image rm") && (strings.Contains(l, "-f") || !strings.HasPrefix(l, "image rm ghcr.io/heedle-ai/")) {
			t.Errorf("podman %s", l)
		}
	}
	if strings.Contains(string(b), "mine") || strings.Contains(string(b), "rm ghcr.io/heedle-ai-evil") || strings.Contains(string(b), "inuse") {
		t.Errorf("a prune touched something not heedle's, or in use:\n%s", b)
	}
	if _, err := r.PruneImages(ctx, bobbin.HeedleRepo, nil); !errors.Is(err, bobbin.ErrInvalid) {
		t.Errorf("a prune with nothing to keep: %v", err)
	}
	for _, repo := range []string{"", "ghcr.io/", "ghcr.io/heedle-ai", "*", "ghcr.io/*/", "/heedle-ai/", "ghcr.io/heedle-ai/x/"} {
		if _, err := r.Images(ctx, repo, keep); !errors.Is(err, bobbin.ErrInvalid) {
			t.Errorf("repository %q: %v", repo, err)
		}
	}
}

// distinctImage builds a FROM scratch image holding content under tag, so
// each test image has its own ID.
func distinctImage(t *testing.T, content string, tags ...string) {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f"), []byte(content), 0o644)
	os.WriteFile(filepath.Join(dir, "Containerfile"), []byte("FROM scratch\nCOPY f /f\n"), 0o644)
	args := []string{"build", "--quiet"}
	for _, tag := range tags {
		args = append(args, "-t", tag)
	}
	if out, err := exec.Command("podman", append(args, dir)...).CombinedOutput(); err != nil {
		t.Fatalf("podman build: %v\n%s", err, out)
	}
}

// TestContainer_HeedlesOwnOnly: with real podman, heedle's containers are
// listed with their labels, CPU and memory, and removing all of them
// leaves a container of your own running; a prune removes only old
// unused names under heedle's repository, keeps pinned and used ones,
// and leaves your own image and your own name on a shared one (BOB-503,
// BOB-505, BOB-506).
func TestContainer_HeedlesOwnOnly(t *testing.T) {
	img := shellImage(t)
	r := bobbin.Runner{}
	l := bobbin.Labels{Kind: "skill", Workflow: "loom-test/plan", Run: "run-" + t.Name(), Step: "n1"}
	ss, err := r.Start(ctx, bobbin.Spec{Image: img, Labels: l})
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	mine, err := exec.Command("podman", "run", "-d", "--rm", "--entrypoint", "/bin/sh", img, "-c", "sleep 300").Output()
	if err != nil {
		t.Fatal(err)
	}
	mineID := strings.TrimSpace(string(mine))
	defer exec.Command("podman", "rm", "-f", "-t", "0", mineID).Run()
	cs, err := r.Containers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range cs {
		if c.ID == mineID {
			t.Error("your own container was listed as heedle's")
		}
		if c.Name == ss.Name() {
			found = c.Labels == l && c.State == "running" && c.CPU != "" && c.Memory != ""
		}
	}
	if !found {
		t.Errorf("the session isn't listed with its labels and stats: %+v", cs)
	}
	gone, err := r.Remove(ctx, func(c bobbin.Container) bool { return c.Labels.Run == l.Run })
	if err != nil || len(gone) != 1 || gone[0].Name != ss.Name() {
		t.Fatalf("Remove = %+v, %v", gone, err)
	}
	if exec.Command("podman", "container", "exists", ss.Name()).Run() == nil {
		t.Error("the session survived its removal")
	}
	if out, _ := exec.Command("podman", "inspect", "--format", "{{.State.Running}}", mineID).Output(); strings.TrimSpace(string(out)) != "true" {
		t.Error("removing heedle's containers stopped yours")
	}

	var rnd [4]byte
	rand.Read(rnd[:])
	repo := "localhost/heedle-ai-test-" + hex.EncodeToString(rnd[:]) + "/"
	distinctImage(t, "pinned", repo+"agent:new")
	distinctImage(t, "old", repo+"agent:old")
	distinctImage(t, "inuse", repo+"browser:inuse")
	distinctImage(t, "shared", repo+"browser:shared", "localhost/mine-"+hex.EncodeToString(rnd[:])+":shared")
	defer func() {
		for _, n := range []string{repo + "agent:new", repo + "agent:old", repo + "browser:inuse", repo + "browser:shared", "localhost/mine-" + hex.EncodeToString(rnd[:]) + ":shared"} {
			exec.Command("podman", "image", "rm", "-f", n).Run()
		}
	}()
	ctr, err := exec.Command("podman", "create", repo+"browser:inuse", "/f").Output()
	if err != nil {
		t.Fatal(err)
	}
	defer exec.Command("podman", "rm", "-f", strings.TrimSpace(string(ctr))).Run()
	removed, err := r.PruneImages(ctx, repo, []string{repo + "agent:new"})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(removed)
	if want := []string{repo + "agent:old", repo + "browser:shared"}; !reflect.DeepEqual(removed, want) {
		t.Errorf("removed %v, want %v", removed, want)
	}
	for n, want := range map[string]bool{repo + "agent:new": true, repo + "agent:old": false, repo + "browser:inuse": true,
		repo + "browser:shared": false, "localhost/mine-" + hex.EncodeToString(rnd[:]) + ":shared": true} {
		if got := exec.Command("podman", "image", "exists", n).Run() == nil; got != want {
			t.Errorf("after the prune, %s exists: %v, want %v", n, got, want)
		}
	}
}

// TestInv_StoppedStepsContainersAreRemoved: a run stopped (its step
// cancelled, a deadline) or writing too much is removed by the name it was
// run under, whatever its first process does with SIGTERM; a run that ends
// on its own isn't (BOB-508).
func TestInv_StoppedStepsContainersAreRemoved(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log")
	for _, c := range []struct {
		name, run string
		cancel    bool
		removed   bool
	}{
		{"cancelled", `trap 'exit 143' TERM; while :; do sleep 0.05; done`, true, true},
		{"too much output", `head -c 2000000 /dev/zero; trap 'exit 143' TERM; while :; do sleep 0.05; done`, false, true},
		{"ends on its own", `echo ok`, false, false},
	} {
		os.Remove(log)
		podman := fakePodman(t, `echo "$@" >> `+log+`
case "$1" in run) cat >/dev/null; `+c.run+`;; esac`)
		cctx, cancel := context.WithCancel(ctx)
		if c.cancel {
			time.AfterFunc(300*time.Millisecond, cancel)
		}
		_, err := bobbin.Runner{Podman: podman}.Run(cctx, bobbin.Spec{Image: zeros})
		cancel()
		b, _ := os.ReadFile(log)
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		name := regexp.MustCompile(`--name (bobbin-run-[0-9a-f]{16})`).FindStringSubmatch(lines[0])
		if name == nil {
			t.Fatalf("%s: the run had no name: %q", c.name, lines[0])
		}
		rm := "rm --force --time 0 --ignore " + name[1]
		if got := slices.Contains(lines, rm); got != c.removed {
			t.Errorf("%s (%v): removed by name %v, want %v:\n%s", c.name, err, got, c.removed, b)
		}
	}
}

// TestContainer_StoppedStepLeavesNoContainer: with real podman, a step
// stopped while its container's first process ignores SIGTERM (sleep as
// PID 1) leaves no container behind (BOB-508).
func TestContainer_StoppedStepLeavesNoContainer(t *testing.T) {
	img := shellImage(t)
	run := fmt.Sprintf("stop-%d", time.Now().UnixNano())
	cctx, cancel := context.WithCancel(ctx)
	time.AfterFunc(2*time.Second, cancel)
	start := time.Now()
	_, err := bobbin.Runner{}.Run(cctx, bobbin.Spec{Image: img, Args: []string{"/bin/sleep", "600"}, Labels: bobbin.Labels{Run: run}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want cancelled", err)
	}
	if d := time.Since(start); d > 45*time.Second {
		t.Errorf("a stopped step took %v to end", d)
	}
	out, _ := exec.Command("podman", "ps", "--all", "--quiet", "--filter", "label=heedle.run="+run).Output()
	if strings.TrimSpace(string(out)) != "" {
		exec.Command("podman", "rm", "-f", "-t", "0", strings.TrimSpace(string(out))).Run()
		t.Errorf("a stopped step's container outlived it: %s", out)
	}
}
