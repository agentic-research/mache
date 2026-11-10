// mcp-probe calls MCP tools on a running mache, or any MCP server, as a real
// client would, and reports what each call answered and how long it took. It
// is the live counterpart of internal/mcpharness, which it is built on: one
// client for tests, release parity and hand checks alike, instead of a
// script per occasion (mache-2d21b5).
//
//	go run ./tools/mcp-probe -url http://localhost:7532/mcp -project <token> \
//	    '[["get_overview",{}],["find_definition",{"symbol":"Open"}]]'
//	go run ./tools/mcp-probe -stdio bin/mache -root . -- serve --stdio . '[["get_overview",{}]]'
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
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "mcp-probe:", err)
		os.Exit(1)
	}
}

// call is one [tool, args] pair of the plan.
type call struct {
	Tool string
	Args map[string]any
}

func (c *call) UnmarshalJSON(b []byte) error {
	var pair []json.RawMessage
	if err := json.Unmarshal(b, &pair); err != nil || len(pair) < 1 || len(pair) > 2 {
		return fmt.Errorf("a call is [tool] or [tool, {args}], got %s", b)
	}
	if err := json.Unmarshal(pair[0], &c.Tool); err != nil {
		return fmt.Errorf("tool name: %w", err)
	}
	if len(pair) == 2 {
		if err := json.Unmarshal(pair[1], &c.Args); err != nil {
			return fmt.Errorf("args of %s: %w", c.Tool, err)
		}
	}
	return nil
}

// run parses args, connects, makes the calls and writes one line per call.
func run(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("mcp-probe", flag.ContinueOnError)
	endpoint := fs.String("url", "", "Streamable HTTP endpoint, e.g. http://localhost:7532/mcp")
	stdio := fs.String("stdio", "", "server binary to run over stdio; its arguments follow --")
	project := fs.String("project", "", "?project= token to send (HTTP)")
	root := fs.String("root", "", "workspace root to answer roots/list with")
	listen := fs.Bool("listen", false, "hold the standalone GET stream (HTTP, legacy protocol)")
	protocol := fs.String("protocol", "legacy", "protocol revision: legacy (2025-11-25) or modern (2026-07-28)")
	show := fs.Bool("show", false, "print each answer's text")
	timeout := fs.Duration("timeout", 2*time.Minute, "per-call timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return errors.New(`want the calls as the last argument: '[["tool",{"arg":1}],...]'`)
	}
	var plan []call
	if err := json.Unmarshal([]byte(rest[len(rest)-1]), &plan); err != nil {
		return fmt.Errorf("calls: %w", err)
	}
	b := mcpharness.Behavior{Project: *project, Listen: *listen}
	if *root != "" {
		b.Roots = []string{*root}
	}
	switch *protocol {
	case "legacy":
		b.Protocol = mcpharness.Legacy
	case "modern":
		b.Protocol = mcpharness.Modern
	default:
		return fmt.Errorf("-protocol %q: want legacy or modern", *protocol)
	}

	hctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	var conn *mcpharness.Conn
	var err error
	switch {
	case *endpoint != "" && *stdio == "":
		conn, err = mcpharness.ConnectHTTP(hctx, *endpoint, b)
	case *stdio != "" && *endpoint == "":
		conn, err = mcpharness.ConnectStdio(hctx, *stdio, os.Environ(), rest[:len(rest)-1], b)
	default:
		return errors.New("give exactly one of -url or -stdio")
	}
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	for _, c := range plan {
		cctx, ccancel := context.WithTimeout(ctx, *timeout)
		start := time.Now()
		text, isError, err := conn.Call(cctx, c.Tool, c.Args)
		took := time.Since(start)
		ccancel()
		status := "ok "
		switch {
		case err != nil:
			status, text = "ERR", err.Error()
		case isError:
			status = "ERR"
		}
		argJSON, _ := json.Marshal(c.Args)
		fmt.Fprintf(out, "%8.3fs %s %s %s %dB\n", took.Seconds(), status, c.Tool, argJSON, len(text))
		if *show || status == "ERR" {
			fmt.Fprintln(out, indent(text))
		}
	}
	return nil
}

func indent(s string) string {
	return "    " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n    ")
}
