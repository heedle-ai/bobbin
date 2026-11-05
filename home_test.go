package bobbin_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"

	"github.com/heedle-ai/bobbin"
)

// TestInv_EveryContainerSaysItsHome: a container started under a context
// that carries a home (WithHome, as a worker sets it for all its
// activities) is labelled heedle.home with it, one-shot or session, with
// or without run labels, and a step's labels read it too; HomeOf is 12
// hex digits of the store's absolute path, the same however the path is
// written, and different for another store; a home that isn't 12 hex
// digits is refused before Podman runs (BOB-520, roadmap 9.2.1).
func TestInv_EveryContainerSaysItsHome(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "store.git")
	os.MkdirAll(store, 0o700)
	home := bobbin.HomeOf(store)
	if !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(home) || !bobbin.ValidHome(home) {
		t.Fatalf("HomeOf = %q", home)
	}
	// The same store, written another way: relative, through a link.
	os.Symlink(store, filepath.Join(dir, "link.git"))
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(dir)
	for _, p := range []string{"store.git", "./store.git/", filepath.Join(dir, "link.git")} {
		if got := bobbin.HomeOf(p); got != home {
			t.Errorf("HomeOf(%q) = %q, want %q", p, got, home)
		}
	}
	if bobbin.HomeOf(filepath.Join(t.TempDir(), "store.git")) == home {
		t.Error("two stores have the same home")
	}

	podman := fakePodman(t, `for a in "$@"; do printf '%s\n' "$a"; done`)
	r := bobbin.Runner{Podman: podman}
	hctx := bobbin.WithHome(ctx, home)
	run := bobbin.Labels{Workflow: "loom-abc/plan", Run: "6f1c2a7d-3b8e", Step: "n2"}
	for _, c := range []struct {
		name string
		ctx  context.Context
		l    bobbin.Labels
		want []string
	}{
		{"one-shot", hctx, bobbin.Labels{}, []string{"heedle.kind=container", "heedle.home=" + home}},
		{"one-shot in a run", bobbin.WithLabels(hctx, run), bobbin.Labels{}, []string{"heedle.kind=container", "heedle.workflow=loom-abc/plan", "heedle.run=6f1c2a7d-3b8e", "heedle.step=n2", "heedle.home=" + home}},
		{"a connector's, with labels of its own", bobbin.WithLabels(hctx, run), bobbin.Labels{Kind: "connector", Workflow: "w", Run: "r"}, []string{"heedle.kind=connector", "heedle.workflow=w", "heedle.run=r", "heedle.home=" + home}},
		{"the home in the run labels", bobbin.WithLabels(ctx, bobbin.Labels{Step: "n1", Home: home}), bobbin.Labels{}, []string{"heedle.kind=container", "heedle.step=n1", "heedle.home=" + home}},
		{"no home", ctx, bobbin.Labels{}, []string{"heedle.kind=container"}},
	} {
		out, err := r.Run(c.ctx, bobbin.Spec{Image: zeros, Env: map[string]string{"A": "1"}, Labels: c.l})
		if err != nil {
			t.Fatal(c.name, err)
		}
		got, placed := labelsIn(argsOf(out), zeros)
		if !reflect.DeepEqual(got, c.want) || !placed {
			t.Errorf("%s: labels %q (placed: %v), want %q", c.name, got, placed, c.want)
		}
	}
	log := filepath.Join(t.TempDir(), "log")
	podman = fakePodman(t, `[ "$1" = run ] && for a in "$@"; do printf '%s\n' "$a"; done >> `+log+`; exit 0`)
	ss, err := bobbin.Runner{Podman: podman}.Start(hctx, bobbin.Spec{Image: zeros, Labels: bobbin.Labels{Kind: "skill"}})
	if err != nil {
		t.Fatal(err)
	}
	ss.Close()
	b, _ := os.ReadFile(log)
	if got, _ := labelsIn(argsOf(b), zeros); !reflect.DeepEqual(got, []string{"heedle.kind=skill", "heedle.home=" + home}) {
		t.Errorf("a session's labels: %q", got)
	}

	// A worker's activities start from its context: a step's labels say
	// the home.
	var s testsuite.WorkflowTestSuite
	env := s.NewTestActivityEnvironment()
	env.SetWorkerOptions(worker.Options{BackgroundActivityContext: bobbin.WithHome(context.Background(), home)})
	env.RegisterActivityWithOptions(func(ctx context.Context) (string, error) {
		b, err := json.Marshal(bobbin.StepLabels(ctx, "n3"))
		return string(b), err
	}, activity.RegisterOptions{Name: "Read"})
	v, err := env.ExecuteActivity("Read")
	if err != nil {
		t.Fatal(err)
	}
	var out string
	v.Get(&out)
	var l bobbin.Labels
	json.Unmarshal([]byte(out), &l)
	if l.Home != home || l.Step != "n3" || l.Run == "" {
		t.Errorf("a step's labels under a worker's home: %+v", l)
	}
	if l := bobbin.StepLabels(bobbin.WithHome(ctx, "--privileged"), "n3"); l.Home != "" {
		t.Errorf("StepLabels kept a bad home: %+v", l)
	}

	// A home that isn't one is refused, given or carried.
	marker := filepath.Join(t.TempDir(), "ran")
	podman = fakePodman(t, "touch "+marker)
	for _, h := range []string{"--privileged", "ABCDEF012345", "0123456789a", "0123456789abc", "/home/me/.heedle", "0123456789ab --x"} {
		if _, err := (bobbin.Runner{Podman: podman}).Run(ctx, bobbin.Spec{Image: zeros, Labels: bobbin.Labels{Home: h}}); !errors.Is(err, bobbin.ErrInvalid) {
			t.Errorf("Run with home %q: %v, want ErrInvalid", h, err)
		}
		if _, err := (bobbin.Runner{Podman: podman}).Start(bobbin.WithHome(ctx, h), bobbin.Spec{Image: zeros}); !errors.Is(err, bobbin.ErrInvalid) {
			t.Errorf("Start with context home %q: %v, want ErrInvalid", h, err)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("podman ran for a home that should have been refused")
	}
}

// TestInv_ContainersSayTheirHome: Containers reads each container's home
// label; one without it has none (a container from before roadmap
// 9.2.1), and one whose label isn't 12 hex digits is InvalidHome, never
// taken for one without a label (BOB-521).
func TestInv_ContainersSayTheirHome(t *testing.T) {
	a, b, c := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	podman := fakePodman(t, `case "$1" in ps) cat <<'END'
[{"Id":"`+a+`","Names":["a"],"State":"exited","Created":1,"Labels":{"heedle.kind":"agent","heedle.home":"0123456789ab"}},
 {"Id":"`+b+`","Names":["b"],"State":"exited","Created":2,"Labels":{"heedle.kind":"agent"}},
 {"Id":"`+c+`","Names":["c"],"State":"exited","Created":3,"Labels":{"heedle.kind":"agent","heedle.home":"--privileged"}}]
END
;; esac`)
	cs, err := bobbin.Runner{Podman: podman}.Containers(ctx)
	if err != nil || len(cs) != 3 {
		t.Fatalf("Containers = %+v, %v", cs, err)
	}
	if cs[0].Labels.Home != "0123456789ab" || cs[1].Labels.Home != "" || cs[2].Labels.Home != bobbin.InvalidHome || bobbin.ValidHome(bobbin.InvalidHome) {
		t.Errorf("homes: %q %q %q", cs[0].Labels.Home, cs[1].Labels.Home, cs[2].Labels.Home)
	}
}

// TestInv_HomeOfIsTheSameBeforeTheStoreExists: a store not made yet,
// reached through a symbolic link, has the label it will have once it is
// made (BOB-522, review 55: heedle up labelled a fresh home by its
// unresolved path, and the worker by its resolved one).
func TestInv_HomeOfIsTheSameBeforeTheStoreExists(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(link, "home", "store.git")
	before := bobbin.HomeOf(store)
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatal(err)
	}
	if after := bobbin.HomeOf(store); before != after || after != bobbin.HomeOf(filepath.Join(real, "home", "store.git")) {
		t.Errorf("HomeOf before the store exists %q, after %q, by its real path %q", before, after, bobbin.HomeOf(filepath.Join(real, "home", "store.git")))
	}
}
