// Command upper is the smallest useful bobbin agent: it reads its upstream
// outputs from standard input and writes them, upper-cased and joined in
// node-ID order, to standard output.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

func main() {
	var upstream map[string]string // node ID -> that node's output
	if err := json.NewDecoder(os.Stdin).Decode(&upstream); err != nil {
		fmt.Fprintln(os.Stderr, "upper:", err)
		os.Exit(1) // non-zero: the step fails, and Temporal retries it in a fresh container
	}
	ids := make([]string, 0, len(upstream))
	for id := range upstream {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var parts []string
	for _, id := range ids {
		parts = append(parts, strings.ToUpper(upstream[id]))
	}
	fmt.Println(strings.Join(parts, " "))
}
