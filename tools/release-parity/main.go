// Command release-parity runs two mache binaries through the same MCP tool
// calls on the same corpus and reports every difference in what they answer.
//
// It is the behavior half of a release gate. `task check` proves the tree is
// internally consistent; it cannot say what changed for someone upgrading from
// the last release. This does: the release binary and the candidate each serve
// the corpus over stdio, every call in a committed plan goes to both, and the
// answers are compared. Each difference must then be explained by a CHANGELOG
// entry, or it is a regression.
//
// Three properties carry the weight:
//
//   - EXHAUSTIVE: the run fails if the candidate registers a tool the plan never
//     calls, so a new tool cannot ship unexercised.
//   - NOISE IS MEASURED, NOT GUESSED: each binary runs the plan [runsPerSide]
//     times, and any JSON path whose value (or presence) differs between runs
//     of the SAME binary is masked before the cross-version comparison. No list
//     of "volatile-looking" field names.
//   - ISOLATED: each binary gets its own HOME, so neither reads the other's
//     parse cache. The version-namespaced leyline binaries are linked in, so no
//     run downloads anything.
//
// Usage:
//
//	release-parity -base ./mache-v0.21.1 -head bin/mache \
//	  -corpus testdata/snapshots/small-go-golden \
//	  -plan tools/release-parity/plans/small-go-golden.json -out .parity/small-go
//	release-parity -head bin/mache -corpus ... -list   # print the registry
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// schema is passed through to `mache serve --schema` when set, so a plan can
// exercise a schema projection as well as the raw one.
var schema string

// call is one entry in a plan.
type call struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

// answer is what one binary returned for one call, normalized.
type answer struct {
	// Missing is true when the binary does not register the tool at all.
	Missing bool `json:"missing,omitempty"`
	IsError bool `json:"is_error,omitempty"`
	// Body is the decoded JSON when the text content parses as JSON, and the
	// raw text otherwise.
	Body any `json:"body"`
}

func main() {
	base := flag.String("base", "", "release binary to compare against")
	head := flag.String("head", "", "candidate binary")
	corpus := flag.String("corpus", "", "directory both binaries serve")
	planPath := flag.String("plan", "", "JSON array of {tool, args}")
	out := flag.String("out", ".parity", "directory for the report and raw answers")
	list := flag.Bool("list", false, "print the head binary's tools and input schemas, then exit")
	timeout := flag.Duration("timeout", 5*time.Minute, "per-binary deadline, including the initial index")
	flag.StringVar(&schema, "schema", "", "topology schema passed to `serve --schema` (default: the raw projection)")
	flag.Parse()

	if *head == "" || *corpus == "" {
		log.Fatal("release-parity: -head and -corpus are required")
	}
	root, err := filepath.Abs(*corpus)
	if err != nil {
		log.Fatal(err)
	}
	if *list {
		tools, err := listTools(*head, root, *timeout)
		if err != nil {
			log.Fatal(err)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(tools); err != nil {
			log.Fatal(err)
		}
		return
	}
	if *base == "" || *planPath == "" {
		log.Fatal("release-parity: -base and -plan are required unless -list")
	}
	plan, err := readPlan(*planPath)
	if err != nil {
		log.Fatal(err)
	}
	code, err := compareReleases(*base, *head, root, plan, *out, *timeout)
	if err != nil {
		log.Fatal(err)
	}
	os.Exit(code)
}

// compareReleases executes the plan on both binaries and writes the report. The exit code
// is 0 when every answer matches, 1 when any differs, and 2 when the plan does
// not cover the head binary's registry.
func compareReleases(base, head, root string, plan []call, out string, timeout time.Duration) (int, error) {
	headTools, err := listTools(head, root, timeout)
	if err != nil {
		return 0, fmt.Errorf("head: %w", err)
	}
	if uncovered := uncoveredTools(headTools, plan); len(uncovered) > 0 {
		fmt.Fprintf(os.Stderr, "release-parity: the plan never calls %s; add a call for each before trusting this run\n",
			strings.Join(uncovered, ", "))
		return 2, nil
	}

	answers := map[string][]answer{}
	for _, side := range []struct{ name, bin string }{{"base", base}, {"head", head}} {
		var masked []answer
		for run := range runsPerSide {
			got, err := answerPlan(side.bin, root, plan, timeout)
			if err != nil {
				return 0, fmt.Errorf("%s (run %d): %w", side.name, run+1, err)
			}
			if masked == nil {
				masked = got
				continue
			}
			masked = maskNoise(masked, got)
		}
		answers[side.name] = masked
	}
	// A path that is noise on either side is noise in the comparison.
	for i := range plan {
		answers["base"][i].Body, answers["head"][i].Body = maskEither(answers["base"][i].Body, answers["head"][i].Body)
	}

	if err := os.MkdirAll(out, 0o755); err != nil {
		return 0, err
	}
	if err := writeAnswers(out, plan, answers); err != nil {
		return 0, err
	}
	changed, err := writeReport(out, base, head, root, plan, answers)
	if err != nil {
		return 0, err
	}
	fmt.Printf("release-parity: %d of %d calls differ; report at %s\n", changed, len(plan), filepath.Join(out, "report.md"))
	if changed > 0 {
		return 1, nil
	}
	return 0, nil
}

func listTools(bin, root string, timeout time.Duration) (tools []mcp.Tool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	s, err := openSession(ctx, bin, root)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, s.close()) }()
	res, err := s.c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	slices.SortFunc(res.Tools, func(a, b mcp.Tool) int { return strings.Compare(a.Name, b.Name) })
	return res.Tools, nil
}

func answerPlan(bin, root string, plan []call, timeout time.Duration) (answers []answer, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	s, err := openSession(ctx, bin, root)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, s.close()) }()
	listed, err := s.c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	has := map[string]bool{}
	for _, t := range listed.Tools {
		has[t.Name] = true
	}

	out := make([]answer, len(plan))
	for i, c := range plan {
		if !has[c.Tool] {
			out[i] = answer{Missing: true}
			continue
		}
		req := mcp.CallToolRequest{}
		req.Params.Name = c.Tool
		req.Params.Arguments = c.Args
		res, err := s.c.CallTool(ctx, req)
		if err != nil {
			// A transport-level failure is an answer too: the comparison is
			// about what a client sees.
			out[i] = answer{IsError: true, Body: normalizeText(err.Error(), s.root, s.home)}
			continue
		}
		out[i] = answer{IsError: res.IsError, Body: normalizeText(textOf(res), s.root, s.home)}
	}
	return out, nil
}

func textOf(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if t, ok := mcp.AsTextContent(c); ok {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// normalizeText replaces the run-specific roots, then decodes JSON when the
// text is JSON so key order and whitespace cannot register as a difference.
func normalizeText(s, root, home string) any {
	// Each root is replaced in its symlink-resolved form first, because mache
	// reports canonical paths (on macOS, TMPDIR's /var is /private/var). And
	// root lives inside home, so it goes before home.
	for _, r := range []struct{ path, name string }{{root, "$CORPUS"}, {home, "$HOME"}} {
		if resolved, err := filepath.EvalSymlinks(r.path); err == nil && resolved != r.path {
			s = strings.ReplaceAll(s, resolved, r.name)
		}
		s = strings.ReplaceAll(s, r.path, r.name)
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err == nil {
		return v
	}
	return s
}

func uncoveredTools(tools []mcp.Tool, plan []call) []string {
	called := map[string]bool{}
	for _, c := range plan {
		called[c.Tool] = true
	}
	var missing []string
	for _, t := range tools {
		if !called[t.Name] {
			missing = append(missing, t.Name)
		}
	}
	return missing
}

func readPlan(path string) ([]call, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var plan []call
	if err := json.Unmarshal(b, &plan); err != nil {
		return nil, fmt.Errorf("plan %s: %w", path, err)
	}
	if len(plan) == 0 {
		return nil, errors.New("plan is empty")
	}
	return plan, nil
}
