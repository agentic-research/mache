// mcp-probe calls MCP tools on a running mache, or any MCP server, as a real
// client would, and reports what each call answered and how long it took. It
// is the live counterpart of internal/mcpharness, which it is built on: one
// client for tests, release parity and hand checks alike, instead of a
// script per occasion (mache-2d21b5). A plan is the release-parity format, so
// a parity plan file runs here as it is.
//
//	go run ./tools/mcp-probe -url http://localhost:7532/mcp -project <token> \
//	    '[{"tool":"get_overview"},{"tool":"find_definition","args":{"symbol":"Open"}}]'
//	go run ./tools/mcp-probe -url http://localhost:7532/mcp -project <token> \
//	    -plan tools/release-parity/plans/small-go-golden.json
//	go run ./tools/mcp-probe -stdio bin/mache '[{"tool":"get_overview"}]' -- serve --stdio .
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/agentic-research/mache/internal/mcpharness"
)

func main() {
	if err := execute(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "mcp-probe:", err)
		os.Exit(1)
	}
}

// probeOptions is a parsed command line.
type probeOptions struct {
	endpoint  string
	stdio     string
	stdioArgs []string
	behavior  mcpharness.Behavior
	show      bool
	timeout   time.Duration
	plan      []mcpharness.Call
}

// execute connects as the command line says, makes the plan's calls and
// writes one line per call to out.
func execute(ctx context.Context, args []string, out io.Writer) error {
	o, err := parseArgs(args)
	if err != nil {
		return err
	}
	conn, err := dial(ctx, o)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	return probe(ctx, o, conn, out)
}

// parseArgs reads the flags, the plan (an inline JSON argument or -plan
// file) and, after --, a stdio server's own arguments.
func parseArgs(args []string) (probeOptions, error) {
	var o probeOptions
	if i := indexOf(args, "--"); i >= 0 {
		args, o.stdioArgs = args[:i], args[i+1:]
	}
	fs := flag.NewFlagSet("mcp-probe", flag.ContinueOnError)
	fs.StringVar(&o.endpoint, "url", "", "Streamable HTTP endpoint, e.g. http://localhost:7532/mcp")
	fs.StringVar(&o.stdio, "stdio", "", "server binary to run over stdio; its arguments follow --")
	fs.StringVar(&o.behavior.Project, "project", "", "?project= token to send (HTTP)")
	root := fs.String("root", "", "workspace root to answer roots/list with")
	fs.BoolVar(&o.behavior.Listen, "listen", false, "hold the standalone GET stream (HTTP, legacy protocol)")
	protocol := fs.String("protocol", "legacy", "protocol revision: legacy (2025-11-25) or modern (2026-07-28)")
	planFile := fs.String("plan", "", "plan file (release-parity format) instead of an inline plan")
	fs.BoolVar(&o.show, "show", false, "print each answer's text")
	fs.DurationVar(&o.timeout, "timeout", 2*time.Minute, "per-call timeout")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if (o.endpoint == "") == (o.stdio == "") {
		return o, errors.New("give exactly one of -url or -stdio")
	}
	if *root != "" {
		o.behavior.Roots = []string{*root}
	}
	switch *protocol {
	case "legacy":
		o.behavior.Protocol = mcpharness.Legacy
	case "modern":
		o.behavior.Protocol = mcpharness.Modern
	default:
		return o, fmt.Errorf("-protocol %q: want legacy or modern", *protocol)
	}
	raw, err := planSource(*planFile, fs.Args())
	if err != nil {
		return o, err
	}
	o.plan, err = mcpharness.ParsePlan(raw)
	if err != nil {
		return o, fmt.Errorf("plan: %w", err)
	}
	return o, nil
}

// planSource is the plan's JSON: the -plan file, or the one inline argument.
func planSource(file string, rest []string) ([]byte, error) {
	switch {
	case file != "" && len(rest) == 0:
		return os.ReadFile(file)
	case file == "" && len(rest) == 1:
		return []byte(rest[0]), nil
	default:
		return nil, errors.New(`want one plan: -plan <file> or '[{"tool":"...","args":{...}}]'`)
	}
}

func dial(ctx context.Context, o probeOptions) (*mcpharness.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	if o.endpoint != "" {
		return mcpharness.ConnectHTTP(ctx, o.endpoint, o.behavior)
	}
	return mcpharness.ConnectStdio(ctx, o.stdio, os.Environ(), o.stdioArgs, o.behavior)
}

// probe makes each call and writes its time, status, call and answer size;
// the answer itself with -show, and always for an error.
func probe(ctx context.Context, o probeOptions, conn *mcpharness.Conn, out io.Writer) error {
	for _, c := range o.plan {
		cctx, cancel := context.WithTimeout(ctx, o.timeout)
		start := time.Now()
		text, isError, err := conn.Call(cctx, c.Tool, c.Args)
		took := time.Since(start)
		cancel()
		status := "ok "
		if err != nil {
			status, text = "ERR", err.Error()
		} else if isError {
			status = "ERR"
		}
		args, _ := json.Marshal(c.Args)
		if _, err := fmt.Fprintf(out, "%8.3fs %s %s %s %dB\n", took.Seconds(), status, c.Tool, args, len(text)); err != nil {
			return err
		}
		if o.show || status == "ERR" {
			if _, err := fmt.Fprintln(out, "    "+strings.ReplaceAll(strings.TrimRight(text, "\n"), "\n", "\n    ")); err != nil {
				return err
			}
		}
	}
	return nil
}

func indexOf(args []string, s string) int {
	for i, a := range args {
		if a == s {
			return i
		}
	}
	return -1
}
