// Command agent is the test image's entrypoint. Its first argument picks a
// behaviour; see podman_test.go.
package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"
	"time"
)

func main() {
	stdin, _ := io.ReadAll(os.Stdin)
	mode := ""
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	switch mode {
	case "echo":
		fmt.Printf("args=%q stdin=%s node=%s attempt=%s\n", os.Args[2:], stdin, os.Getenv("HEEDLE_NODE"), os.Getenv("HEEDLE_ATTEMPT"))
	case "scratch":
		// Report whether an earlier run left /scratch behind, then leave it.
		fmt.Println(state())
		os.WriteFile("/scratch", []byte("left behind"), 0o644)
	case "flaky":
		// Attempt 1 dirties the container and fails; later attempts report
		// whether they can see that.
		if os.Getenv("HEEDLE_ATTEMPT") == "1" {
			os.WriteFile("/scratch", []byte("left behind"), 0o644)
			fmt.Fprintln(os.Stderr, "attempt 1 fails on purpose")
			os.Exit(3)
		}
		fmt.Print(state())
	case "net":
		ifs, _ := net.Interfaces()
		var names []string
		for _, i := range ifs {
			names = append(names, i.Name)
		}
		fmt.Printf("interfaces: %s\n", strings.Join(names, " "))
		c, err := net.DialTimeout("tcp", "1.1.1.1:53", 2*time.Second)
		if err != nil {
			fmt.Println("network: unreachable")
			return
		}
		c.Close()
		fmt.Println("network: reachable")
	case "env":
		var names []string
		for _, kv := range os.Environ() {
			k, _, _ := strings.Cut(kv, "=")
			names = append(names, k)
		}
		sort.Strings(names)
		fmt.Println(strings.Join(names, " "))
	case "fail":
		fmt.Fprintln(os.Stderr, "something went wrong")
		os.Exit(7)
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q\n", mode)
		os.Exit(2)
	}
}

func state() string {
	if _, err := os.Stat("/scratch"); err == nil {
		return "dirty"
	}
	return "clean"
}
