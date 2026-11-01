package bobbin_test

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestInv_GettingStartedClonesEverySibling: the clone loop on Getting
// Started names exactly this repo and every sibling that go.mod builds
// against, so following it gives a checkout that builds. (Its list had
// fallen behind once bobbin and reed were added.)
func TestInv_GettingStartedClonesEverySibling(t *testing.T) {
	mod, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^module github\.com/heedle-ai/([a-z]+)$|github\.com/heedle-ai/([a-z]+) => \.\./`).FindAllStringSubmatch(string(mod), -1) {
		want[m[1]+m[2]] = true
	}
	page, err := os.ReadFile("docs/wiki/Getting-Started.md")
	if err != nil {
		t.Fatal(err)
	}
	loops := regexp.MustCompile(`for r in ([a-z ]+); do git clone https://github\.com/heedle-ai/\$r\.git; done`).FindAllStringSubmatch(string(page), -1)
	if len(loops) != 1 {
		t.Fatalf("Getting Started has %d clone loops, want 1", len(loops))
	}
	got := map[string]bool{}
	for _, r := range strings.Fields(loops[0][1]) {
		got[r] = true
	}
	if g, w := cloneSet(got), cloneSet(want); g != w {
		t.Errorf("Getting Started clones %s; go.mod needs %s", g, w)
	}
}

func cloneSet(m map[string]bool) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, " ")
}
