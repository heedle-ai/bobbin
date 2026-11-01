package bobbin_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"github.com/heedle-ai/bobbin"
	"github.com/heedle-ai/draft"
	"github.com/heedle-ai/draft/registry"
	"github.com/heedle-ai/shuttle/plan"
)

// runProgram compiles a draft program that may use container, runs it with
// shuttle's workflow in Temporal's test environment, and returns the result.
func runProgram(t *testing.T, r bobbin.Runner, src string) (plan.Result, error) {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.SetTestTimeout(2 * time.Minute)
	reg := draft.DefaultRegistry()
	if err := bobbin.Register(reg, env, r); err != nil {
		t.Fatal(err)
	}
	plan.Register(env)
	p, err := draft.Compile(context.Background(), reg, draft.Source(src))
	if err != nil {
		t.Fatal(err)
	}
	env.ExecuteWorkflow(plan.WorkflowName, p)
	var res plan.Result
	if err := env.GetWorkflowError(); err != nil {
		return res, err
	}
	return res, env.GetWorkflowResult(&res)
}

// The container command composes like any other: upstream outputs arrive
// on stdin as JSON, keyed by node ID.
func TestContainer_InAProgram(t *testing.T) {
	img := testImage(t)
	res, err := runProgram(t, bobbin.Runner{}, `(pipe (par (echo "a") (echo "b")) (container "`+img+`" "echo" "x"))`)
	if err != nil {
		t.Fatal(err)
	}
	want := `args=["x"] stdin={"n0":"a","n1":"b"} node=n2 attempt=1` + "\n"
	if res.Outputs["n2"] != want {
		t.Errorf("n2 = %q, want %q", res.Outputs["n2"], want)
	}
}

// Temporal retries a failed step in a brand-new container: attempt 1
// writes /scratch and fails; attempt 2 must not see it.
func TestContainer_RetryRunsInAFreshContainer(t *testing.T) {
	img := testImage(t)
	res, err := runProgram(t, bobbin.Runner{}, `(container "`+img+`" "flaky")`)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["n0"] != "clean" {
		t.Errorf("retry saw %q: the failed attempt's writes survived", res.Outputs["n0"])
	}
}

// Bad input and setup failures are non-retryable, with the documented
// types; no podman is needed to check this.
func TestInv_CommandErrorsAreNonRetryable(t *testing.T) {
	for _, c := range []struct {
		r    bobbin.Runner
		src  string
		want string
	}{
		{bobbin.Runner{}, `(container "alpine:latest")`, "InvalidInput"},
		{bobbin.Runner{Podman: "/nonexistent/podman"}, `(container "` + zeros + `")`, "ContainerSetup"},
		{bobbin.Runner{Podman: fakePodman(t, "exit 125")}, `(container "` + zeros + `")`, "ContainerSetup"},
	} {
		_, err := runProgram(t, c.r, c.src)
		if app := findType(err, c.want); app == nil || !app.NonRetryable() {
			t.Errorf("%s: err = %v, want non-retryable %s", c.src, err, c.want)
		}
	}
}

// The draft command takes an image and any number of arguments.
func TestInv_DraftCommandShape(t *testing.T) {
	reg := registry.New()
	if err := bobbin.Register(reg, nopWorker{}, bobbin.Runner{}); err != nil {
		t.Fatal(err)
	}
	p, err := draft.Compile(context.Background(), reg, draft.Source(`(container "`+zeros+`" "a" "b" "c")`))
	if err != nil {
		t.Fatal(err)
	}
	if n := p.Nodes[0]; n.Activity != bobbin.ActivityName || len(n.Args) != 4 || n.Args[3] != "c" {
		t.Errorf("node = %+v", n)
	}
	if _, err := draft.Compile(context.Background(), reg, draft.Source(`(container)`)); err == nil {
		t.Error("(container) without an image compiled")
	}
}

// findType returns the first Temporal application error of type typ in
// err's chain.
func findType(err error, typ string) *temporal.ApplicationError {
	for ; err != nil; err = errors.Unwrap(err) {
		if app, ok := err.(*temporal.ApplicationError); ok && app.Type() == typ {
			return app
		}
	}
	return nil
}

// The wiki's example agent, built from examples/upper, in a program.
func TestContainer_ExampleAgent(t *testing.T) {
	img := buildImage(t, "./examples/upper", "upper")
	res, err := runProgram(t, bobbin.Runner{}, `(pipe (par (echo "hello") (echo "world")) (container "`+img+`"))`)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Outputs["n2"]; got != "HELLO WORLD\n" {
		t.Errorf("n2 = %q", got)
	}
}
