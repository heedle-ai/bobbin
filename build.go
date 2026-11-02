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
	directive = regexp.MustCompile(`(?i)^\s*#\s*(escape|syntax|check)\s*=`)
)

// CheckContainerfile reports whether a Containerfile is one the image
// command builds, wrapping ErrInvalid if not. It is deliberately strict,
// refusing anything it can't read with certainty, since podman, not
// bobbin, is the parser:
//   - every FROM, and every COPY --from, names "scratch", an earlier stage,
//     or an image pinned to content, so a build always starts from the
//     same bytes; FROM takes no flags;
//   - no ADD (it can fetch URLs and unpack archives: use COPY), no
//     ONBUILD, no flags on RUN (such as --network or --mount), and COPY
//     takes only --from, --chown, --chmod and --link;
//   - no parser directives (# escape=, # syntax=) and no heredocs (<<),
//     which change how lines are read.
func CheckContainerfile(text string) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: Containerfile: "+format, append([]any{ErrInvalid}, a...)...)
	}
	if len(text) > MaxContainerfile || strings.ContainsRune(text, 0) {
		return bad("must be at most %d bytes, without NUL", MaxContainerfile)
	}
	var lines []string
	cur := ""
	for _, l := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if directive.MatchString(l) {
			return bad("parser directives aren't allowed: %q", strings.TrimSpace(l))
		}
		t := strings.TrimSpace(l)
		if cur == "" && (t == "" || strings.HasPrefix(t, "#")) {
			continue
		}
		if strings.HasSuffix(t, `\`) {
			cur += strings.TrimSuffix(t, `\`) + " "
			continue
		}
		lines = append(lines, cur+t)
		cur = ""
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	stages := map[string]bool{}
	from := func(ref string) bool {
		return strings.EqualFold(ref, "scratch") || stages[strings.ToLower(ref)] || pinned.MatchString(ref)
	}
	froms := 0
	for _, l := range lines {
		f := strings.Fields(l)
		inst, args := strings.ToUpper(f[0]), f[1:]
		if strings.Contains(l, "<<") && (inst == "RUN" || inst == "COPY") {
			return bad("heredocs aren't allowed: %q", clipLine(l))
		}
		switch inst {
		case "FROM":
			froms++
			if len(args) == 0 || strings.HasPrefix(args[0], "-") || !from(args[0]) {
				return bad("FROM must name scratch, an earlier stage or an image pinned to content (name@sha256:…), without flags: %q", clipLine(l))
			}
			if len(args) == 3 && strings.EqualFold(args[1], "AS") {
				stages[strings.ToLower(args[2])] = true
			} else if len(args) != 1 {
				return bad("FROM takes an image and an optional AS name: %q", clipLine(l))
			}
		case "ADD", "ONBUILD":
			return bad("%s isn't allowed (use COPY): %q", inst, clipLine(l))
		case "RUN":
			if len(args) > 0 && strings.HasPrefix(args[0], "--") {
				return bad("RUN takes no flags: %q", clipLine(l))
			}
		case "COPY":
			for _, a := range args {
				if !strings.HasPrefix(a, "--") {
					break
				}
				switch {
				case strings.HasPrefix(a, "--from="):
					if !from(strings.TrimPrefix(a, "--from=")) {
						return bad("COPY --from must name an earlier stage or an image pinned to content: %q", clipLine(l))
					}
				case strings.HasPrefix(a, "--chown="), strings.HasPrefix(a, "--chmod="), a == "--link":
				default:
					return bad("COPY takes only --from, --chown, --chmod and --link: %q", clipLine(l))
				}
			}
		}
		if froms == 0 && inst != "ARG" {
			return bad("must start with FROM")
		}
	}
	if froms == 0 {
		return bad("has no FROM")
	}
	return nil
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
// command refuses fail with "InvalidInput", a push with no registry
// configured with "ImageSetup", both non-retryable; a build or push that
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
		if errors.Is(err, ErrInvalid) {
			return "", invalid(err)
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
