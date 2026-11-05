package bobbin

import (
	"context"
	"fmt"
	"regexp"

	"go.temporal.io/sdk/activity"
)

// heedle's labels (roadmap 9.2). Every container heedle starts carries
// LabelKind; one started for a step of a run also carries the workflow
// and run that scheduled it, as Temporal reports them, and the step's
// node ID. They're how heedle finds its own containers, and the only way
// it tells them from anything else on the machine: heedle stops or
// removes only containers that carry them.
const (
	LabelKind     = "heedle.kind"
	LabelWorkflow = "heedle.workflow"
	LabelRun      = "heedle.run"
	LabelStep     = "heedle.step"
)

// Kinds are the kinds of container heedle starts: an agent step's
// workspace, a skill step's, a container step's, a connector's MCP
// server, a browse step's browser and an act step's browser.
var Kinds = []string{"agent", "skill", "container", "connector", "browse", "act"}

// Labels are one container's heedle labels. Workflow, Run and Step are
// empty outside a run.
type Labels struct {
	Kind     string `json:"kind"`
	Workflow string `json:"workflow,omitempty"`
	Run      string `json:"run,omitempty"`
	Step     string `json:"step,omitempty"`
}

// labelValue is what a label's value may be: printable ASCII with no
// space, quote, comma or equals sign, starting with a letter or digit, at
// most 255 bytes. Temporal's workflow and run IDs and loom's node IDs fit.
// A value is always joined to its name in one argument (--label
// heedle.step=VALUE), after the fixed flags, so it can never be read as a
// flag; the pattern keeps it plain besides.
var labelValue = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,254}$`)

// ValidLabelValue reports whether v may be a label's value.
func ValidLabelValue(v string) bool { return labelValue.MatchString(v) }

// KnownKind reports whether k is one of Kinds.
func KnownKind(k string) bool {
	for _, x := range Kinds {
		if k == x {
			return true
		}
	}
	return false
}

// Validate reports whether l can be given to podman: Kind empty or one of
// Kinds, and each other value empty or a valid label value.
func (l Labels) Validate() error {
	if l.Kind != "" && !KnownKind(l.Kind) {
		return fmt.Errorf("label %s=%q isn't one of %v", LabelKind, l.Kind, Kinds)
	}
	for name, v := range map[string]string{LabelWorkflow: l.Workflow, LabelRun: l.Run, LabelStep: l.Step} {
		if v != "" && !ValidLabelValue(v) {
			return fmt.Errorf("label %s=%q has characters a label can't", name, v)
		}
	}
	return nil
}

// Of is l with kind k.
func (l Labels) Of(k string) Labels { l.Kind = k; return l }

// Map is l as podman's labels: name to value, the empty ones left out.
func (l Labels) Map() map[string]string {
	m := map[string]string{}
	for _, kv := range [][2]string{{LabelKind, l.Kind}, {LabelWorkflow, l.Workflow}, {LabelRun, l.Run}, {LabelStep, l.Step}} {
		if kv[1] != "" {
			m[kv[0]] = kv[1]
		}
	}
	return m
}

// args are l's --label flags, in a fixed order, with kind def when l
// names none: so every container carries heedle.kind.
func (l Labels) args(def string) []string {
	if l.Kind == "" {
		l.Kind = def
	}
	var argv []string
	for _, kv := range [][2]string{{LabelKind, l.Kind}, {LabelWorkflow, l.Workflow}, {LabelRun, l.Run}, {LabelStep, l.Step}} {
		if kv[1] != "" {
			argv = append(argv, "--label", kv[0]+"="+kv[1])
		}
	}
	return argv
}

// orFrom is l, with the run labels ctx carries (WithLabels) when l has
// none of its own.
func (l Labels) orFrom(ctx context.Context) Labels {
	if l.Workflow == "" && l.Run == "" && l.Step == "" {
		c := LabelsFrom(ctx)
		l.Workflow, l.Run, l.Step = c.Workflow, c.Run, c.Step
	}
	return l
}

type labelsKey struct{}

// WithLabels is ctx carrying a step's run labels, for every container
// started under it that has none of its own: loom sets them where a
// step's activity starts (loom.Labelled), from Temporal's activity info.
func WithLabels(ctx context.Context, l Labels) context.Context {
	return context.WithValue(ctx, labelsKey{}, l)
}

// LabelsFrom is the run labels ctx carries, if any.
func LabelsFrom(ctx context.Context) Labels {
	l, _ := ctx.Value(labelsKey{}).(Labels)
	return l
}

// StepLabels are the run labels of the step node, read from Temporal's
// activity info when ctx is an activity's: the workflow that scheduled
// it and that workflow's run. A value a label can't hold is left out, so
// labelling never fails a step.
func StepLabels(ctx context.Context, step string) Labels {
	var l Labels
	if activity.IsActivity(ctx) {
		e := activity.GetInfo(ctx).WorkflowExecution
		l.Workflow, l.Run = e.ID, e.RunID
	}
	l.Step = step
	for _, v := range []*string{&l.Workflow, &l.Run, &l.Step} {
		if !ValidLabelValue(*v) {
			*v = ""
		}
	}
	return l
}
