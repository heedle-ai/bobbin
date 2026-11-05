package bobbin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Seeing and tidying heedle's containers and images (roadmap 9.2). Each is
// one podman command; nothing here stops or removes anything that doesn't
// carry heedle's labels (containers) or isn't named in heedle's repository
// (images).

// Container is one of heedle's containers, as podman lists it.
type Container struct {
	ID      string    `json:"id"` // podman's full container ID
	Name    string    `json:"name"`
	Image   string    `json:"image"`
	Labels  Labels    `json:"labels"`
	Created time.Time `json:"created"`
	State   string    `json:"state"` // podman's: running, exited, …
	// CPU and Memory are podman stats' figures for a running container
	// ("12.5%", "94kB / 536.9MB"); empty when podman couldn't say.
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

var fullID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Containers lists heedle's containers: those labelled heedle.kind with
// one of Kinds. Labels with values a label can't hold are left out. CPU
// and memory come from podman stats, once; if it can't say (a container
// gone in between), they're left empty.
func (r Runner) Containers(ctx context.Context) ([]Container, error) {
	out, err := r.output(ctx, "ps", "--all", "--filter", "label="+LabelKind, "--format", "json")
	if err != nil {
		return nil, err
	}
	var ps []struct {
		ID      string `json:"Id"`
		Names   []string
		Image   string
		Labels  map[string]string
		Created int64
		State   string
	}
	if err := json.Unmarshal(out, &ps); err != nil {
		return nil, fmt.Errorf("podman ps: %w", err)
	}
	cs := []Container{}
	var running []string
	for _, p := range ps {
		l := Labels{Kind: p.Labels[LabelKind], Workflow: p.Labels[LabelWorkflow], Run: p.Labels[LabelRun], Step: p.Labels[LabelStep]}
		if !KnownKind(l.Kind) || !fullID.MatchString(p.ID) {
			continue
		}
		for _, v := range []*string{&l.Workflow, &l.Run, &l.Step} {
			if !ValidLabelValue(*v) {
				*v = ""
			}
		}
		c := Container{ID: p.ID, Image: p.Image, Labels: l, Created: time.Unix(p.Created, 0).UTC(), State: p.State}
		if len(p.Names) > 0 {
			c.Name = p.Names[0]
		}
		if c.State == "running" {
			running = append(running, c.ID)
		}
		cs = append(cs, c)
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].Created.Before(cs[j].Created) })
	if len(running) > 0 {
		if out, err := r.output(ctx, append([]string{"stats", "--no-stream", "--format", "json"}, running...)...); err == nil {
			var st []struct {
				ID     string `json:"id"`
				CPU    string `json:"cpu_percent"`
				Memory string `json:"mem_usage"`
			}
			if json.Unmarshal(out, &st) == nil {
				for i := range cs {
					for _, s := range st {
						if s.ID != "" && strings.HasPrefix(cs[i].ID, s.ID) {
							cs[i].CPU, cs[i].Memory = s.CPU, s.Memory
						}
					}
				}
			}
		}
	}
	return cs, nil
}

// Remove lists heedle's containers (Containers) and removes those which
// picks, at once, returning them. It can only remove what that listing
// found: nothing without heedle's labels, whatever a caller asks
// (BOB-503).
func (r Runner) Remove(ctx context.Context, which func(Container) bool) ([]Container, error) {
	cs, err := r.Containers(ctx)
	if err != nil {
		return nil, err
	}
	var gone []Container
	args := []string{"rm", "--force", "--time", "0", "--ignore"}
	for _, c := range cs {
		if which(c) {
			gone = append(gone, c)
			args = append(args, c.ID)
		}
	}
	if len(gone) == 0 {
		return []Container{}, nil
	}
	if _, err := r.output(ctx, args...); err != nil {
		return nil, err
	}
	return gone, nil
}

// Image is one image podman has that heedle's repository names.
type Image struct {
	ID         string    `json:"id"`
	Names      []string  `json:"names"` // its names in the repository: repo:tag or repo@sha256:…
	Size       int64     `json:"size"`  // bytes
	Created    time.Time `json:"created"`
	Containers int       `json:"containers"` // containers using it, running or not
	// Pinned: one of its names or digests is a ref heedle keeps (the
	// current release's, or the worker's).
	Pinned bool `json:"pinned"`
}

// HeedleRepo is where heedle's own images live; only images named there
// are ever pruned.
const HeedleRepo = "ghcr.io/heedle-ai/"

// repoPrefix is what a repository prefix may be: a registry host and an
// organization, ending in a slash, so it can never mean "every image".
var repoPrefix = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*(:[0-9]+)?/[a-z0-9][a-z0-9._-]*/$`)

// Images lists the images podman has that are named under repo (such as
// HeedleRepo), marking those whose name or digest is in keep.
func (r Runner) Images(ctx context.Context, repo string, keep []string) ([]Image, error) {
	if !repoPrefix.MatchString(repo) {
		return nil, fmt.Errorf("%w: repository %q must be a registry and an organization, ending in /", ErrInvalid, repo)
	}
	out, err := r.output(ctx, "images", "--format", "json")
	if err != nil {
		return nil, err
	}
	var all []struct {
		ID          string `json:"Id"`
		Names       []string
		RepoDigests []string
		Size        int64
		Created     int64
		Containers  int
	}
	if err := json.Unmarshal(out, &all); err != nil {
		return nil, fmt.Errorf("podman images: %w", err)
	}
	kept := map[string]bool{}
	for _, k := range keep {
		kept[k] = true
	}
	ims := []Image{}
	seen := map[string]bool{}
	for _, a := range all {
		// podman lists an image once for each of its names (BOB-505).
		if seen[a.ID] {
			continue
		}
		seen[a.ID] = true
		im := Image{ID: a.ID, Size: a.Size, Created: time.Unix(a.Created, 0).UTC(), Containers: a.Containers, Names: []string{}}
		for _, n := range a.Names {
			if strings.HasPrefix(n, repo) {
				im.Names = append(im.Names, n)
			}
		}
		if len(im.Names) == 0 {
			continue
		}
		for _, n := range append(append([]string{}, a.Names...), a.RepoDigests...) {
			if kept[n] {
				im.Pinned = true
			}
		}
		ims = append(ims, im)
	}
	sort.Slice(ims, func(i, j int) bool { return ims[i].Created.After(ims[j].Created) })
	return ims, nil
}

// PruneImages removes heedle's old images: for each image named under
// repo that isn't pinned (keep) and that no container uses, it removes
// its names under repo, one at a time and never forced. An image that
// also has a name elsewhere (your own tag) keeps it, and podman keeps the
// image; one with no name left is deleted. keep must name at least one
// image, so a prune can't remove the release's own. It returns the names
// it removed.
func (r Runner) PruneImages(ctx context.Context, repo string, keep []string) ([]string, error) {
	if len(keep) == 0 {
		return nil, fmt.Errorf("%w: pruning needs the images to keep", ErrInvalid)
	}
	ims, err := r.Images(ctx, repo, keep)
	if err != nil {
		return nil, err
	}
	removed := []string{}
	for _, im := range ims {
		if im.Pinned || im.Containers > 0 {
			continue
		}
		for _, n := range im.Names {
			// No --force: podman refuses an image a container started
			// since uses (BOB-505).
			if _, err := r.output(ctx, "image", "rm", n); err != nil {
				return removed, err
			}
			removed = append(removed, n)
		}
	}
	return removed, nil
}

// Machine is Podman's virtual machine on macOS, as podman machine list
// reports it.
type Machine struct {
	Name    string `json:"name"`
	Running bool   `json:"running"`
	CPUs    uint64 `json:"cpus"`
	Memory  uint64 `json:"memory"` // bytes
	Disk    uint64 `json:"disk"`   // bytes
}

// Machine is the machine podman uses: the running one, else the default;
// nil when there is none (on Linux, podman needs none).
func (r Runner) Machine(ctx context.Context) (*Machine, error) {
	out, err := r.output(ctx, "machine", "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	// Memory and DiskSize are bytes, as strings (podman 4.9 and 5).
	var ms []struct {
		Name             string
		Default, Running bool
		CPUs             uint64
		Memory, DiskSize string
	}
	if err := json.Unmarshal(out, &ms); err != nil {
		return nil, fmt.Errorf("podman machine list: %w", err)
	}
	var pick *Machine
	for _, m := range ms {
		mem, _ := strconv.ParseUint(m.Memory, 10, 64)
		disk, _ := strconv.ParseUint(m.DiskSize, 10, 64)
		this := &Machine{Name: m.Name, Running: m.Running, CPUs: m.CPUs, Memory: mem, Disk: disk}
		if m.Running || pick == nil || (m.Default && !pick.Running) {
			pick = this
		}
		if m.Running {
			break
		}
	}
	return pick, nil
}

// output runs podman with args and returns its standard output, or an
// error that says what podman said.
func (r Runner) output(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.podman(), args...)
	cmd.Env = cleanEnv(os.Environ())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("podman %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
