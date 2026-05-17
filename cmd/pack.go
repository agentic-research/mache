package cmd

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
	machetmpl "github.com/agentic-research/mache/internal/template"
	"github.com/spf13/cobra"
)

// pack produces a markdown context bundle suitable for pre-injection into
// an agent prompt. Composes three existing capabilities — overview,
// architecture, community summary — into a single self-contained doc.
//
// The idea (bead mache-33ffb0): GrapeRoot's measured win over vanilla
// Claude comes from paying the structural-work cost ONCE per session via
// pre-injection. Mache produces richer structural data than GrapeRoot but
// has historically shipped it only via on-demand MCP tools — which can
// be net-negative on agent workflows (GrapeRoot's own MCP-DGC mode was
// MORE expensive than vanilla). `mache pack` is the missing layer.
//
// Usage:
//
//	mache pack <db>           # full bundle to stdout
//	mache pack <db> --max=20  # cap entries per section (default 25)
//
// Pipe it into an agent session:
//
//	BUNDLE=$(mache pack /tmp/repo.db)
//	claude -p "..." --append-system-prompt "$BUNDLE"
var packCmd = &cobra.Command{
	Use:   "pack [db]",
	Short: "Produce a markdown context bundle for agent pre-injection",
	Long: `Emit a single self-contained markdown document describing the codebase
projected by the .db file: top-level structure, key abstractions (entry
points, most-defined symbols, API surface), dependency layers (community
detection summary), and tool routing hints.

Designed for piping into an agent session via --append-system-prompt or
equivalent. Pays the structural-work cost once at session start, so the
agent doesn't need to discover this via on-demand tool calls per prompt.`,
	Args: cobra.ExactArgs(1),
	RunE: runPack,
}

var (
	packMaxEntries int
	packDir        string
)

func init() {
	packCmd.Flags().IntVar(&packMaxEntries, "max", 25, "Maximum entries per section (communities, abstractions, API surface)")
	packCmd.Flags().StringVar(&packDir, "dir", "", "Optional human-readable name for the codebase root (default: basename of db path)")
	rootCmd.AddCommand(packCmd)
}

func runPack(_ *cobra.Command, args []string) error {
	dbPath := args[0]
	if _, err := os.Stat(dbPath); err != nil {
		return fmt.Errorf("db not found: %w", err)
	}

	g, err := graph.OpenSQLiteGraph(dbPath, &api.Topology{}, machetmpl.Render)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = g.Close() }()

	name := packDir
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(dbPath), ".db")
	}

	out := bufio.NewWriter(os.Stdout)
	defer func() { _ = out.Flush() }()

	if err := writePackBundle(out, g, name, packMaxEntries); err != nil {
		return err
	}
	return nil
}

// writePackBundle composes the three sections into a single markdown
// document. Order matters: overview first (so the agent knows the corpus
// size), then architecture (entry points), then communities (deeper
// structural lay of the land), then tool routing (how to query for more).
func writePackBundle(w *bufio.Writer, g graph.Graph, name string, maxEntries int) error {
	_, _ = fmt.Fprintf(w, "# Codebase context: %s\n\n", name)
	_, _ = fmt.Fprintf(w, "Pre-injected structural summary from `mache pack`. Authoritative for layout and entry points; use `find_callers` / `find_definition` / `read_file` for specific symbol lookups.\n\n")

	if err := writeOverviewSection(w, g); err != nil {
		return err
	}
	if err := writeArchitectureSection(w, g, maxEntries); err != nil {
		return err
	}
	writeCommunitiesSection(w, g, maxEntries)
	writeToolRoutingSection(w)
	return nil
}

func writeOverviewSection(w *bufio.Writer, g graph.Graph) error {
	roots, err := g.ListChildren("")
	if err != nil {
		return fmt.Errorf("list root: %w", err)
	}

	var topDirs []string
	totalDirs, totalFiles := 0, 0
	for _, id := range roots {
		node, err := g.GetNode(id)
		if err != nil {
			continue
		}
		if node.Mode.IsDir() {
			totalDirs++
			subs, _ := g.ListChildren(id)
			topDirs = append(topDirs, fmt.Sprintf("- `%s/` (%d children)", filepath.Base(id), len(subs)))
		} else {
			totalFiles++
		}
	}

	refTokens, defTokens := 0, 0
	if rp, ok := g.(refsMapProvider); ok {
		refTokens = len(rp.RefsMap())
	}
	if dp, ok := g.(defsMapProvider); ok {
		defTokens = len(dp.DefsMap())
	}

	_, _ = fmt.Fprintf(w, "## Top-level structure\n\n")
	_, _ = fmt.Fprintf(w, "%d top-level dirs, %d top-level files. %d unique ref tokens, %d unique def tokens indexed.\n\n",
		totalDirs, totalFiles, refTokens, defTokens)
	for _, line := range topDirs {
		_, _ = fmt.Fprintln(w, line)
	}
	_, _ = fmt.Fprintln(w)
	return nil
}

func writeArchitectureSection(w *bufio.Writer, g graph.Graph, maxEntries int) error {
	// Refs map → most-referenced symbols (entry points by fan-in).
	rp, ok := g.(refsMapProvider)
	if !ok {
		_, _ = fmt.Fprintf(w, "## Architecture\n\n_No cross-reference data available; skipping._\n\n")
		return nil
	}
	refs := rp.RefsMap()
	if len(refs) == 0 {
		_, _ = fmt.Fprintf(w, "## Architecture\n\n_Cross-reference index is empty; skipping._\n\n")
		return nil
	}

	type tokenCount struct {
		token string
		count int
	}
	counts := make([]tokenCount, 0, len(refs))
	for token, nodeIDs := range refs {
		counts = append(counts, tokenCount{token, len(nodeIDs)})
	}
	sort.Slice(counts, func(i, j int) bool { return counts[i].count > counts[j].count })

	_, _ = fmt.Fprintf(w, "## Architecture\n\n### Most-referenced symbols (entry points by fan-in)\n\n")
	for _, tc := range counts[:min(len(counts), maxEntries)] {
		_, _ = fmt.Fprintf(w, "- `%s` (%d refs)\n", tc.token, tc.count)
	}
	_, _ = fmt.Fprintln(w)

	// Defs map → key abstractions + API surface.
	dp, ok := g.(defsMapProvider)
	if !ok {
		return nil
	}
	defs := dp.DefsMap()
	if len(defs) == 0 {
		return nil
	}

	type symDef struct {
		symbol string
		ids    []string
	}
	syms := make([]symDef, 0, len(defs))
	for symbol, ids := range defs {
		syms = append(syms, symDef{symbol, ids})
	}
	sort.Slice(syms, func(i, j int) bool { return len(syms[i].ids) > len(syms[j].ids) })

	_, _ = fmt.Fprintf(w, "### Key abstractions (most-defined symbols)\n\n")
	const maxIDsPerSym = 4
	for _, sd := range syms[:min(len(syms), maxEntries)] {
		ids := sd.ids
		more := 0
		if len(ids) > maxIDsPerSym {
			more = len(ids) - maxIDsPerSym
			ids = ids[:maxIDsPerSym]
		}
		suffix := ""
		if more > 0 {
			suffix = fmt.Sprintf(" (+%d more)", more)
		}
		_, _ = fmt.Fprintf(w, "- `%s` defined at: `%s`%s\n", sd.symbol, strings.Join(ids, "`, `"), suffix)
	}
	_, _ = fmt.Fprintln(w)

	// API surface: exported symbols (uppercase first rune).
	var exported []string
	for symbol := range defs {
		if len(symbol) == 0 {
			continue
		}
		r, _ := utf8.DecodeRuneInString(symbol)
		if unicode.IsUpper(r) {
			exported = append(exported, symbol)
		}
	}
	sort.Strings(exported)
	if len(exported) > 0 {
		_, _ = fmt.Fprintf(w, "### API surface (top %d exported symbols, %d total)\n\n", min(len(exported), maxEntries*2), len(exported))
		shown := min(len(exported), maxEntries*2)
		for i := range shown {
			if i > 0 && i%8 == 0 {
				_, _ = fmt.Fprintln(w)
			}
			_, _ = fmt.Fprintf(w, "`%s` ", exported[i])
		}
		_, _ = fmt.Fprint(w, "\n\n")
	}
	return nil
}

func writeCommunitiesSection(w *bufio.Writer, g graph.Graph, maxEntries int) {
	rp, ok := g.(refsMapProvider)
	if !ok {
		return
	}
	refs := rp.RefsMap()
	if len(refs) == 0 {
		return
	}
	// Skip community detection on very large graphs — Louvain is O(n^2)
	// in worst case and we want pack to be fast even on big corpora.
	const communityLimit = 5000
	if len(refs) > communityLimit {
		_, _ = fmt.Fprintf(w, "## Dependency layers\n\n_Skipped: ref token count (%d) exceeds the %d threshold for fast community detection. Run `mcp get_communities --summary=true` if needed._\n\n", len(refs), communityLimit)
		return
	}

	result := graph.DetectCommunities(refs, 2)
	if len(result.Communities) == 0 {
		return
	}

	// Largest-first.
	sort.Slice(result.Communities, func(i, j int) bool {
		return len(result.Communities[i].Members) > len(result.Communities[j].Members)
	})

	cap := min(len(result.Communities), maxEntries)
	_, _ = fmt.Fprintf(w, "## Dependency layers (top %d of %d communities, %d-node graph, modularity %.3f)\n\n",
		cap, len(result.Communities), result.NumNodes, result.Modularity)
	const maxMembersPerCommunity = 5
	for i, c := range result.Communities[:cap] {
		top := c.Members
		if len(top) > maxMembersPerCommunity {
			top = top[:maxMembersPerCommunity]
		}
		cleaned := make([]string, len(top))
		for j, m := range top {
			cleaned[j] = strings.TrimSuffix(m, "/source")
		}
		_, _ = fmt.Fprintf(w, "%d. cluster of %d (e.g. `%s`)\n", i+1, len(c.Members), strings.Join(cleaned, "`, `"))
	}
	_, _ = fmt.Fprintln(w)
}

func writeToolRoutingSection(w *bufio.Writer) {
	_, _ = fmt.Fprintln(w, "## How to query this codebase further")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "When you need specifics beyond this overview, prefer mache's structural tools over `grep` / `Read`:")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "- `find_callers(token)` — who calls a symbol; cleaner than grep for usage lookup")
	_, _ = fmt.Fprintln(w, "- `find_definition(symbol)` — where a symbol is declared; faster than grep on common names")
	_, _ = fmt.Fprintln(w, "- `find_callees(path)` — what a construct invokes; lists resolved targets")
	_, _ = fmt.Fprintln(w, "- `search(pattern, role=\"definition\")` — pattern-match symbol definitions (use percent-Foo-percent or similar SQL LIKE)")
	_, _ = fmt.Fprintln(w, "- `list_directory(path)` — browse construct trees (functions, methods, types as dirs)")
	_, _ = fmt.Fprintln(w, "- `read_file(path)` — fetch a specific construct's source")
	_, _ = fmt.Fprintln(w, "- `get_impact(symbol)` — blast-radius BFS for change-impact analysis")
	_, _ = fmt.Fprintln(w, "- `get_diagram` — mermaid quotient graph of communities")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Use `Read` / `Grep` only for free-text searches (log strings, magic constants) or non-code files.")
}

// rootCmd, refsMapProvider, defsMapProvider are defined in serve_registry.go.
// min was added to the stdlib in Go 1.21 — we use 1.25, so the builtin works.
