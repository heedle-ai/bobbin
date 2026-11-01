package bobbin

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/heedle-ai/draft/registry"
	"github.com/heedle-ai/shuttle"
	"github.com/heedle-ai/shuttle/plan"
	"github.com/heedle-ai/weft"
)

// Names under which Register adds the command.
const (
	CommandName  = "container" // in draft programs
	ActivityName = "Container" // in Temporal
)

// Command is the container command as an arrow: a plan node's Args[0] is
// the image and Args[1:] its arguments. The container reads, on standard
// input, a JSON object mapping each upstream node ID to its output ({}
// when there is none), and gets HEEDLE_NODE (this node's ID) and
// HEEDLE_ATTEMPT (Temporal's attempt number, from 1; 1 outside an
// activity) in its environment. Its standard output is the node's output.
//
// Errors are classified for Temporal: an invalid spec fails with the
// non-retryable type "InvalidInput", a container Podman can't start with
// "ContainerSetup", one whose output exceeds MaxOutput with
// "OutputTooLarge", and a command that exits non-zero with a retryable
// error, so Temporal retries it in a fresh container.
func Command(r Runner) weft.Arrow[plan.Input, string] {
	return func(ctx context.Context, in plan.Input) (string, error) {
		if len(in.Args) == 0 {
			return "", temporal.NewNonRetryableApplicationError("container needs an image", "InvalidInput", nil)
		}
		up := in.Upstream
		if up == nil {
			up = map[string]string{}
		}
		stdin, err := json.Marshal(up) // map keys are written sorted
		if err != nil {
			return "", err
		}
		attempt := int32(1)
		if activity.IsActivity(ctx) {
			attempt = activity.GetInfo(ctx).Attempt
		}
		out, err := r.Run(ctx, Spec{
			Image: in.Args[0],
			Args:  in.Args[1:],
			Env:   map[string]string{"HEEDLE_NODE": in.NodeID, "HEEDLE_ATTEMPT": strconv.Itoa(int(attempt))},
			Stdin: stdin,
		})
		switch {
		case errors.Is(err, ErrInvalid):
			return "", temporal.NewNonRetryableApplicationError(err.Error(), "InvalidInput", nil)
		case errors.Is(err, ErrOutputTooLarge):
			return "", temporal.NewNonRetryableApplicationError(err.Error(), "OutputTooLarge", nil)
		case errors.Is(err, ErrSetup):
			return "", temporal.NewNonRetryableApplicationError(err.Error(), "ContainerSetup", nil)
		case err != nil:
			return "", err
		}
		return string(out), nil
	}
}

// BuiltinSpec is the draft builtin for the container command: an image, then
// any number of string arguments, on the temporal backend.
func BuiltinSpec() registry.BuiltinSpec {
	return registry.BuiltinSpec{
		Name:    CommandName,
		AgentID: ActivityName,
		ArgSchema: registry.ArgSchema{
			Params:       []registry.ParamSpec{{Name: "image", Type: registry.StringT}},
			Variadic:     true,
			VariadicType: registry.StringT,
		},
		Backends: []registry.Backend{registry.BackendTemporal},
	}
}

// Activities is the part of a Temporal worker Register needs.
type Activities interface {
	RegisterActivityWithOptions(a any, options activity.RegisterOptions)
}

// Register adds the container command to a draft registry and its
// activity to a worker.
func Register(reg *registry.Registry, w Activities, r Runner) error {
	if err := reg.Register(BuiltinSpec()); err != nil {
		return err
	}
	w.RegisterActivityWithOptions(shuttle.ArrowAsActivity(Command(r)), activity.RegisterOptions{Name: ActivityName})
	return nil
}
