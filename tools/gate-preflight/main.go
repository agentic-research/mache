// gate-preflight is a hard gate over mache's Taskfile: it fails the build when
// a gate task (verify / audit / sign / lint / check / validate / smells /
// release / image / build …) invokes an external binary with no preflight
// guard. A missing tool then no-ops the gate into a cryptic failure — or, when
// the invocation is swallowed, a silent false PASS.
//
// The check is `internal/lint.CheckTaskfile`; a guard is any of a task-level
// `preconditions: command -v <tool>`, an inline `command -v`/`which`/`type`/
// `hash`, a `go install …`, or a standalone `test -f`/`test -x` artifact
// assertion. See that package's doc for the full model.
//
// Usage:
//
//	gate-preflight [Taskfile.yml]
//
// Exits 0 when every gate's external binaries are guarded; exits 1 with a
// per-gap report otherwise. The Taskfile path defaults to ./Taskfile.yml.
// Wired into `task gates:preflight`, which the `check` and `ci` gates invoke.
package main

import (
	"fmt"
	"os"

	"github.com/agentic-research/mache/internal/lint"
)

func main() {
	path := "Taskfile.yml"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}

	gaps, err := lint.CheckTaskfile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gate-preflight: %v\n", err)
		os.Exit(1)
	}

	if len(gaps) == 0 {
		fmt.Printf("gate-preflight: OK — every gate task's external tools are guarded (%s)\n", path)
		return
	}

	fmt.Fprintf(os.Stderr, "gate-preflight: %d unguarded gate tool(s) in %s:\n", len(gaps), path)
	for _, g := range gaps {
		fmt.Fprintf(os.Stderr, "  task %-24s tool %-16s (in: %s)\n", g.Task, g.Tool, g.Cmd)
	}
	fmt.Fprintln(os.Stderr, "\nAdd a guard to each gate task — e.g.:")
	fmt.Fprintln(os.Stderr, "  preconditions:")
	fmt.Fprintln(os.Stderr, "    - sh: command -v <tool>")
	fmt.Fprintln(os.Stderr, "      msg: \"<tool> required — install: <url>\"")
	os.Exit(1)
}
