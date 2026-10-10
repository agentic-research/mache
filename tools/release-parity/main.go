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
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/mark3labs/mcp-go/client"
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

// session serves a private copy of the corpus with bin over stdio, under a
// private HOME. The copy matters because write_file edits the corpus: without
// it, one run's edits would become the next run's input.
type session struct {
	c    *client.Client
	home string
	root string
}

func openSession(ctx context.Context, bin, corpus string) (*session, error) {
	home, err := isolatedHome()
	if err != nil {
		return nil, err
	}
	root := filepath.Join(home, "corpus")
	if err := os.CopyFS(root, os.DirFS(corpus)); err != nil {
		_ = os.RemoveAll(home)
		return nil, fmt.Errorf("copy corpus: %w", err)
	}
	env := append(os.Environ(), "HOME="+home)
	args := []string{"serve", "--stdio"}
	if schema != "" {
		args = append(args, "--schema", schema)
	}
	c, err := client.NewStdioMCPClient(bin, env, append(args, root)...)
	if err != nil {
		_ = os.RemoveAll(home)
		return nil, err
	}
	init := mcp.InitializeRequest{}
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcp.Implementation{Name: "release-parity", Version: "1"}
	if _, err := c.Initialize(ctx, init); err != nil {
		_ = c.Close()
		_ = os.RemoveAll(home)
		return nil, fmt.Errorf("initialize: %w", err)
	}
	return &session{c: c, home: home, root: root}, nil
}

func (s *session) close() {
	_ = s.c.Close()
	_ = os.RemoveAll(s.home)
}

// isolatedHome makes a HOME whose ~/.mache/bin is the real one, so each binary
// resolves its own pinned leyline without downloading, and whose parse cache
// and daemon state are private to this run.
func isolatedHome() (string, error) {
	// Under /tmp, not TMPDIR: the managed leyline daemon binds a unix socket
	// beneath HOME, macOS caps socket paths at 104 bytes, and TMPDIR there is a
	// long /var/folders/... path. A run under it measures a socket-length
	// failure no typical HOME has, not the code under test (mache-aab4f0).
	home, err := os.MkdirTemp("/tmp", "rp-")
	if err != nil {
		return "", err
	}
	real, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(home, ".mache"), 0o755); err != nil {
		return "", err
	}
	bin := filepath.Join(real, ".mache", "bin")
	if _, err := os.Stat(bin); err == nil {
		if err := os.Symlink(bin, filepath.Join(home, ".mache", "bin")); err != nil {
			return "", err
		}
	}
	return home, nil
}

func listTools(bin, root string, timeout time.Duration) ([]mcp.Tool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	s, err := openSession(ctx, bin, root)
	if err != nil {
		return nil, err
	}
	defer s.close()
	res, err := s.c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	slices.SortFunc(res.Tools, func(a, b mcp.Tool) int { return strings.Compare(a.Name, b.Name) })
	return res.Tools, nil
}

func answerPlan(bin, root string, plan []call, timeout time.Duration) ([]answer, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	s, err := openSession(ctx, bin, root)
	if err != nil {
		return nil, err
	}
	defer s.close()
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

// runsPerSide is how many times each binary runs the plan to measure its own
// noise. Two runs missed a timing race that happened to agree within each side
// (the sheaf subscriber reconnecting); three makes that much rarer, and a
// difference that survives is reported, never hidden.
const runsPerSide = 3

// noise is what a masked value is replaced with.
const noise = "<varies between identical runs>"

// maskNoise returns first with every value that differs from second masked.
func maskNoise(first, second []answer) []answer {
	out := make([]answer, len(first))
	for i := range first {
		out[i] = first[i]
		out[i].Body = mask(first[i].Body, second[i].Body)
	}
	return out
}

// mask returns a, with every leaf (or shape) that differs from b replaced by
// [noise]. Objects are compared key by key and arrays index by index when
// their lengths agree; anything else that differs is masked whole.
func mask(a, b any) any {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			return noise
		}
		out := make(map[string]any, len(av))
		for k, v := range av {
			w, ok := bv[k]
			if !ok {
				out[k] = noise
				continue
			}
			out[k] = mask(v, w)
		}
		// A key only the later run has is noise too: its presence varies.
		for k := range bv {
			if _, ok := av[k]; !ok {
				out[k] = noise
			}
		}
		return out
	case []any:
		bv, ok := b.([]any)
		if !ok || len(bv) != len(av) {
			if cmp.Equal(a, b) {
				return a
			}
			return noise
		}
		out := make([]any, len(av))
		for i := range av {
			out[i] = mask(av[i], bv[i])
		}
		return out
	default:
		if cmp.Equal(a, b) {
			return a
		}
		return noise
	}
}

// maskEither masks, in both a and b, every position either side already
// masked, so a field that is noise for one version is not reported as a
// difference.
func maskEither(a, b any) (any, any) {
	if a == noise || b == noise {
		return noise, noise
	}
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		ao, bo := make(map[string]any, len(am)), make(map[string]any, len(bm))
		for k, v := range am {
			ao[k] = v
		}
		for k, v := range bm {
			bo[k] = v
		}
		for k := range am {
			if w, ok := bm[k]; ok {
				ao[k], bo[k] = maskEither(am[k], w)
			}
		}
		// A key that is noise on one side and absent on the other is a field
		// whose PRESENCE varies between identical runs; drop it from both.
		for k, v := range ao {
			if _, ok := bo[k]; !ok && v == noise {
				delete(ao, k)
			}
		}
		for k, v := range bo {
			if _, ok := ao[k]; !ok && v == noise {
				delete(bo, k)
			}
		}
		return ao, bo
	}
	as, aok := a.([]any)
	bs, bok := b.([]any)
	if aok && bok && len(as) == len(bs) {
		ao, bo := make([]any, len(as)), make([]any, len(bs))
		for i := range as {
			ao[i], bo[i] = maskEither(as[i], bs[i])
		}
		return ao, bo
	}
	return a, b
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

func writeAnswers(out string, plan []call, answers map[string][]answer) error {
	for side, as := range answers {
		dir := filepath.Join(out, side)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		for i, a := range as {
			b, err := json.MarshalIndent(struct {
				Call   call   `json:"call"`
				Answer answer `json:"answer"`
			}{plan[i], a}, "", "  ")
			if err != nil {
				return err
			}
			name := fmt.Sprintf("%03d-%s.json", i, plan[i].Tool)
			if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeReport writes report.md and returns how many calls differ.
func writeReport(out, base, head, root string, plan []call, answers map[string][]answer) (int, error) {
	f, err := os.Create(filepath.Join(out, "report.md"))
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	return writeDiffReport(f, base, head, root, plan, answers)
}

func writeDiffReport(w io.Writer, base, head, root string, plan []call, answers map[string][]answer) (int, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "# Release parity\n\n- base: `%s`\n- head: `%s`\n- corpus: `%s`\n- schema: `%s`\n- calls: %d\n\n", base, head, root, schemaLabel(), len(plan))
	b.WriteString("Diff lines read `-` base, `+` head.\n\n")
	changed := 0
	for i, c := range plan {
		ba, ha := answers["base"][i], answers["head"][i]
		args, _ := json.Marshal(c.Args)
		switch {
		case ba.Missing && ha.Missing:
			fmt.Fprintf(&b, "## %03d `%s` %s: not registered by either binary\n\n", i, c.Tool, args)
			changed++
		case ba.Missing:
			fmt.Fprintf(&b, "## %03d `%s` %s: NEW in head\n\n", i, c.Tool, args)
			changed++
		case ha.Missing:
			fmt.Fprintf(&b, "## %03d `%s` %s: REMOVED in head\n\n", i, c.Tool, args)
			changed++
		default:
			d := cmp.Diff(ba, ha)
			if d == "" {
				continue
			}
			changed++
			fmt.Fprintf(&b, "## %03d `%s` %s\n\n```diff\n%s```\n\n", i, c.Tool, args, d)
		}
	}
	if changed == 0 {
		b.WriteString("Every answer matches.\n")
	}
	_, err := io.WriteString(w, b.String())
	return changed, err
}

func schemaLabel() string {
	if schema == "" {
		return "raw projection"
	}
	return schema
}
