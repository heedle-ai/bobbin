package bobbin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/openshift/imagebuilder/dockerfile/parser"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/heedle-ai/draft/registry"
	"github.com/heedle-ai/shuttle"
	"github.com/heedle-ai/shuttle/plan"
	"github.com/heedle-ai/weft"
)

// Names under which RegisterImage adds the image command.
const (
	ImageCommandName  = "image" // in draft programs
	ImageActivityName = "Image" // in Temporal
)

// BuildCPUSeconds is the CPU time each RUN process may use. It also ends
// a busy loop left running by a cancelled build, which podman doesn't
// stop (see How-It-Works).
const BuildCPUSeconds = "1800"

// BuildMemoryLimit is the memory a build's RUN steps may use: more than a
// step's container, since compilers and package managers run there.
const BuildMemoryLimit = "2g"

// ImageOptions are the image command's Temporal options: a build can take
// far longer than shuttle's default of 3 minutes.
var ImageOptions = plan.Options{Timeout: 30 * time.Minute}

// MaxContainerfile is the longest Containerfile the image command takes.
const MaxContainerfile = 64 << 10

// Build is one image build: a Containerfile, and the files its build
// context holds, in/<node>.txt for each upstream output. Nothing else
// from the host is in the context.
type Build struct {
	Containerfile string
	Inputs        map[string]string // node ID -> its output
}

var (
	nodeName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	// imageRepo is a repository with no tag or digest, the same pattern
	// pirn and loom use for registries.
	imageRepo = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]+)?(/[a-z0-9]+([._-][a-z0-9]+)*)+$`)
	imageID   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// containerfileInstructions are the instructions a Containerfile may use.
// ADD (it fetches URLs and unpacks archives) and ONBUILD aren't among them.
var containerfileInstructions = map[string]bool{
	"from": true, "run": true, "copy": true, "workdir": true, "env": true, "label": true, "cmd": true,
	"entrypoint": true, "user": true, "expose": true, "arg": true, "stopsignal": true, "healthcheck": true,
	"shell": true, "volume": true, "maintainer": true,
}

var stageName = regexp.MustCompile(`^[a-z][a-z0-9_.-]*$`)

// CheckContainerfile reports whether a Containerfile is one the image
// command builds, wrapping ErrInvalid if not. It reads the file with the
// parser podman's builder itself uses (openshift/imagebuilder, at the
// version buildah 1.33, in podman 4.9, uses), so it sees the instructions
// and flags podman will act on, then refuses:
//   - a FROM or COPY --from that isn't "scratch", an earlier stage, or an
//     image pinned to content, or that holds a variable ($), so a build
//     always starts from the same bytes; stage names must be plain
//     ([a-z][a-z0-9_.-]*);
//   - flags on FROM or RUN (such as RUN --network or --mount), and COPY
//     flags beyond --from, --chown, --chmod and --link;
//   - any instruction but FROM, RUN, COPY, WORKDIR, ENV, LABEL, CMD,
//     ENTRYPOINT, USER, EXPOSE, ARG, STOPSIGNAL, HEALTHCHECK, SHELL, VOLUME
//     and MAINTAINER: so no ADD or ONBUILD;
//   - heredocs, and an escape character other than the default.
func CheckContainerfile(text string) error {
	_, err := checkContainerfile(text)
	return err
}

// checkContainerfile is CheckContainerfile, also returning the pinned
// images the build starts from (in FROM and COPY --from), which must be
// on the worker.
func checkContainerfile(text string) (bases []string, err error) {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: Containerfile: "+format, append([]any{ErrInvalid}, a...)...)
	}
	if len(text) > MaxContainerfile || strings.ContainsRune(text, 0) {
		return nil, bad("must be at most %d bytes, without NUL", MaxContainerfile)
	}
	res, err := parser.Parse(strings.NewReader(text))
	if err != nil {
		return nil, bad("%v", err)
	}
	seen := map[string]bool{}
	base := func(ref string) {
		if pinned.MatchString(ref) && !seen[ref] {
			seen[ref] = true
			bases = append(bases, ref)
		}
	}
	if res.EscapeToken != parser.DefaultEscapeToken {
		return nil, bad("the escape directive isn't allowed")
	}
	stages := map[string]bool{}
	pinnedOrStage := func(ref string) bool {
		return !strings.Contains(ref, "$") && (strings.EqualFold(ref, "scratch") || stages[strings.ToLower(ref)] || pinned.MatchString(ref))
	}
	froms := 0
	for _, n := range res.AST.Children {
		var args []string
		for a := n.Next; a != nil; a = a.Next {
			args = append(args, a.Value)
		}
		line := clipLine(strings.TrimSpace(n.Original))
		if !containerfileInstructions[n.Value] {
			return nil, bad("%s isn't allowed (line %d): %q", strings.ToUpper(n.Value), n.StartLine, line)
		}
		if len(n.Heredocs) > 0 {
			return nil, bad("heredocs aren't allowed (line %d)", n.StartLine)
		}
		if froms == 0 && n.Value != "from" && n.Value != "arg" {
			return nil, bad("must start with FROM (line %d)", n.StartLine)
		}
		switch n.Value {
		case "from":
			froms++
			if len(n.Flags) > 0 || len(args) == 0 || !pinnedOrStage(args[0]) {
				return nil, bad("FROM must name scratch, an earlier stage or an image pinned to content (name@sha256:…), without flags or variables (line %d): %q", n.StartLine, line)
			}
			base(args[0])
			switch {
			case len(args) == 3 && strings.EqualFold(args[1], "AS") && stageName.MatchString(strings.ToLower(args[2])):
				stages[strings.ToLower(args[2])] = true
			case len(args) != 1:
				return nil, bad("FROM takes an image and an optional AS name of letters, digits, _ . and - (line %d): %q", n.StartLine, line)
			}
		case "run":
			if len(n.Flags) > 0 {
				return nil, bad("RUN takes no flags (line %d): %q", n.StartLine, line)
			}
		case "copy":
			for _, f := range n.Flags {
				switch {
				case strings.HasPrefix(f, "--from="):
					if !pinnedOrStage(strings.TrimPrefix(f, "--from=")) {
						return nil, bad("COPY --from must name an earlier stage or an image pinned to content, without variables (line %d): %q", n.StartLine, line)
					}
					base(strings.TrimPrefix(f, "--from="))
				case strings.HasPrefix(f, "--chown="), strings.HasPrefix(f, "--chmod="), f == "--link":
				default:
					return nil, bad("COPY takes only --from, --chown, --chmod and --link (line %d): %q", n.StartLine, line)
				}
			}
		case "healthcheck":
		default:
			if len(n.Flags) > 0 {
				return nil, bad("%s takes no flags (line %d): %q", strings.ToUpper(n.Value), n.StartLine, line)
			}
		}
	}
	if froms == 0 {
		return nil, bad("has no FROM")
	}
	return bases, nil
}

func clipLine(l string) string {
	if len(l) > 120 {
		return l[:120] + "…"
	}
	return l
}

// BuildArgv is the exact podman command line that builds the context in
// dir (its Containerfile and in/), writing the image ID to iidfile. No
// pulls, no network for RUN steps, no privilege escalation, bounded
// memory, no host proxy variables, and timestamps fixed at 0 so the same
// build gives the same image. RUN steps keep Podman's default, reduced
// capabilities: package managers need to own and chmod files, so dropping
// all of them, as step containers do, would break ordinary builds.
func BuildArgv(dir, iidfile string) []string {
	return []string{
		"build",
		"--pull=never",                     // only images already present: no surprise downloads
		"--network=none",                   // RUN steps have no network at all
		"--security-opt=no-new-privileges", // setuid binaries can't gain any
		"--memory=" + BuildMemoryLimit,
		"--ulimit", "cpu=" + BuildCPUSeconds, // each RUN process: CPU seconds, so a loop ends
		"--http-proxy=false",     // don't copy the host's proxy variables in
		"--timestamp=0",          // the same build gives the same image
		"--identity-label=false", // nor the builder's version
		"--iidfile", iidfile,
		"--file", dir + "/Containerfile",
		dir,
	}
}

// PushArgv is the exact podman command line that pushes image id to repo,
// tagged sha256-<hex> so registries that delete untagged images keep it,
// writing the pushed manifest's digest to digestfile. plainHTTP is for a
// registry on localhost without TLS (tests).
func PushArgv(id, repo, digestfile string, plainHTTP bool) []string {
	argv := []string{"push", "--format", "oci", "--digestfile", digestfile}
	if plainHTTP {
		argv = append(argv, "--tls-verify=false")
	}
	return append(argv, id, "docker://"+repo+":sha256-"+strings.TrimPrefix(id, "sha256:"))
}

// ImageRegistry is where the image command pushes, when a program asks:
// the worker's choice, with the worker's own podman login.
type ImageRegistry struct {
	Repo      string // such as ghcr.io/acme/heedle-images
	PlainHTTP bool   // for a registry on localhost without TLS (tests)
}

// ImageResult is the image command's output, as JSON.
type ImageResult struct {
	Image  string `json:"image"`            // the image ID, sha256:<hex>
	Pushed string `json:"pushed,omitempty"` // repo@sha256:<manifest digest>, if pushed
}

// ImageCommand is the image command as an arrow: Args[0] is a
// Containerfile (see CheckContainerfile), and an optional Args[1], "push",
// pushes the image to reg. Upstream outputs are in the build context as
// in/<node>.txt. The output is an ImageResult, whose image ID a later
// program can run with the container command.
//
// Errors are classified for Temporal: a Containerfile or arguments the
// command refuses fail with "InvalidInput", a base image the worker
// doesn't have with "ContainerSetup", and a push with no registry
// configured with "ImageSetup", all non-retryable; a build or push that
// fails is retryable.
func ImageCommand(r Runner, reg *ImageRegistry) weft.Arrow[plan.Input, string] {
	return func(ctx context.Context, in plan.Input) (string, error) {
		invalid := func(err error) error {
			return temporal.NewNonRetryableApplicationError(err.Error(), "InvalidInput", nil)
		}
		if len(in.Args) == 0 || len(in.Args) > 2 || len(in.Args) == 2 && in.Args[1] != "push" {
			return "", invalid(errors.New(`image takes a Containerfile, then optionally "push"`))
		}
		push := len(in.Args) == 2
		if push && (reg == nil || !imageRepo.MatchString(reg.Repo)) {
			return "", temporal.NewNonRetryableApplicationError("the worker has no image registry to push to; give it one with -image-registry", "ImageSetup", nil)
		}
		id, err := r.Build(ctx, Build{Containerfile: in.Args[0], Inputs: in.Upstream})
		switch {
		case errors.Is(err, ErrInvalid):
			return "", invalid(err)
		case errors.Is(err, ErrSetup):
			return "", temporal.NewNonRetryableApplicationError(err.Error(), "ContainerSetup", nil)
		}
		if err != nil {
			return "", err
		}
		res := ImageResult{Image: id}
		if push {
			if res.Pushed, err = r.Push(ctx, id, *reg); err != nil {
				return "", err
			}
		}
		b, err := json.Marshal(res)
		return string(b), err
	}
}

// ImageBuiltinSpec is the draft builtin for the image command: a
// Containerfile and an optional "push", on the temporal backend, with
// ImageOptions.
func ImageBuiltinSpec() registry.BuiltinSpec {
	return registry.BuiltinSpec{
		Name:    ImageCommandName,
		AgentID: ImageActivityName,
		ArgSchema: registry.ArgSchema{
			Params:       []registry.ParamSpec{{Name: "containerfile", Type: registry.StringT}},
			Variadic:     true,
			VariadicType: registry.StringT,
		},
		Backends: []registry.Backend{registry.BackendTemporal},
		Options:  ImageOptions,
	}
}

// RegisterImage adds the image command to a draft registry and its
// activity to a worker. reg may be nil: then programs can build but not
// push.
func RegisterImage(dr *registry.Registry, w Activities, r Runner, reg *ImageRegistry) error {
	if err := dr.Register(ImageBuiltinSpec()); err != nil {
		return err
	}
	w.RegisterActivityWithOptions(shuttle.ArrowAsActivity(ImageCommand(r, reg)), activity.RegisterOptions{Name: ImageActivityName})
	return nil
}

// sortedInputs is in's node IDs in order, each checked to be a safe file
// name.
func sortedInputs(in map[string]string) ([]string, error) {
	var ids []string
	for id := range in {
		if !nodeName.MatchString(id) {
			return nil, fmt.Errorf("%w: input node ID %q", ErrInvalid, id)
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}
