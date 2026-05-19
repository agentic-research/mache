package ingest

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/agentic-research/mache/internal/graph"
)

// CallPattern describes one shape of function call for a language.
// The walker translates these into batched SQL JOINs against the _ast table —
// one JOIN per ancestor — to avoid the N-queries-per-file pattern of the
// generic selector path.
//
// Examples:
//
//	Go bare:       OuterKind=call_expression, LeafKind=identifier
//	Go qualified:  OuterKind=call_expression, Ancestors=[selector_expression],
//	               LeafKind=field_identifier, QualifierKind=identifier
type CallPattern struct {
	OuterKind     string   // e.g. "call_expression"
	Ancestors     []string // intermediate kinds between outer and leaf
	LeafKind      string   // node kind whose record is the call name (@call)
	QualifierKind string   // optional sibling-leaf kind for the package qualifier (@pkg); empty when not qualified
}

// callPatternRegistry stores per-language CallPatterns for ASTWalker.
// Each language maps to a slice (multiple shapes per language).
var callPatternRegistry sync.Map // langName → []CallPattern

// RegisterASTCallPatterns registers per-language structured call patterns for
// the batched ExtractCalls / ExtractQualifiedCalls fast path.
func RegisterASTCallPatterns(langName string, patterns []CallPattern) {
	callPatternRegistry.Store(langName, patterns)
}

// ExtractCalls returns deduplicated function-call tokens for the given
// source file. Issues one JOIN-style SQL query per registered pattern,
// avoiding the per-scope query loop of the generic Query path.
//
// Returns (nil, nil) when the language has no registered patterns — the
// caller treats that as "no calls" and falls through to other paths.
func (w *ASTWalker) ExtractCalls(sourcePath, langName string) ([]string, error) {
	raw, ok := callPatternRegistry.Load(langName)
	if !ok {
		return nil, nil
	}
	patterns := raw.([]CallPattern)
	if len(patterns) == 0 {
		return nil, nil
	}

	sourceID := filepath.Base(sourcePath)
	seen := make(map[string]bool)
	var calls []string
	for _, p := range patterns {
		rows, err := w.queryCallPattern(sourceID, p, false)
		if err != nil {
			continue
		}
		for _, r := range rows {
			if r.token != "" && !seen[r.token] {
				seen[r.token] = true
				calls = append(calls, r.token)
			}
		}
	}
	return calls, nil
}

// ExtractQualifiedCalls returns call tokens with optional package qualifiers.
// Patterns whose QualifierKind is non-empty produce QualifiedCall with both
// fields; bare patterns produce QualifiedCall with empty Qualifier.
func (w *ASTWalker) ExtractQualifiedCalls(sourcePath, langName string) ([]graph.QualifiedCall, error) {
	raw, ok := callPatternRegistry.Load(langName)
	if !ok {
		return nil, nil
	}
	patterns := raw.([]CallPattern)

	sourceID := filepath.Base(sourcePath)
	seen := make(map[string]bool)
	var calls []graph.QualifiedCall
	for _, p := range patterns {
		rows, err := w.queryCallPattern(sourceID, p, true)
		if err != nil {
			continue
		}
		for _, r := range rows {
			if r.token == "" {
				continue
			}
			key := r.qualifier + "." + r.token
			if seen[key] {
				continue
			}
			seen[key] = true
			calls = append(calls, graph.QualifiedCall{Token: r.token, Qualifier: r.qualifier})
		}
	}
	return calls, nil
}

// callRow is one extracted call from a single SQL pattern query.
type callRow struct {
	token     string
	qualifier string
}

// queryCallPattern runs a single batched SQL query for the given CallPattern
// and source file. The query JOINs the _ast/nodes tables once per ancestor
// (LeafKind ↔ Ancestors[len-1] ↔ ... ↔ Ancestors[0] ↔ OuterKind) so the
// result set contains one row per matched call, regardless of how many
// scope nodes exist in the file.
//
// When wantQualifier is true and pattern.QualifierKind is non-empty, the
// query also joins a sibling node (same parent as the leaf) of kind
// QualifierKind to populate r.qualifier.
func (w *ASTWalker) queryCallPattern(sourceID string, p CallPattern, wantQualifier bool) ([]callRow, error) {
	if p.OuterKind == "" || p.LeafKind == "" {
		return nil, fmt.Errorf("invalid CallPattern: OuterKind and LeafKind required")
	}

	// Build the ancestor JOIN chain.
	//   leaf (LeafKind)
	//     ↑ parent_id
	//   ancestor[len-1]      (parent of leaf)
	//     ↑
	//   ...
	//   ancestor[0]          (immediate child of outer)
	//     ↑
	//   outer (OuterKind)
	//
	// Each level requires: nodes table for parent_id chain, _ast for kind.
	var sb strings.Builder
	sb.WriteString(`SELECT n_leaf.record AS call_token`)
	if wantQualifier && p.QualifierKind != "" {
		sb.WriteString(`, COALESCE(n_pkg.record, '') AS pkg`)
	} else {
		sb.WriteString(`, '' AS pkg`)
	}
	sb.WriteString(`
		FROM nodes n_leaf
		JOIN _ast a_leaf ON a_leaf.node_id = n_leaf.id`)
	args := []any{}

	// Walk up the ancestor chain (closest-to-leaf first → outer last).
	prev := "n_leaf"
	for i := len(p.Ancestors) - 1; i >= 0; i-- {
		alias := fmt.Sprintf("n_anc%d", i)
		astAlias := fmt.Sprintf("a_anc%d", i)
		fmt.Fprintf(&sb, `
		JOIN nodes %s ON %s.id = %s.parent_id
		JOIN _ast %s ON %s.node_id = %s.id`,
			alias, alias, prev, astAlias, astAlias, alias)
		prev = alias
	}
	// Finally JOIN the outer scope.
	fmt.Fprintf(&sb, `
		JOIN nodes n_outer ON n_outer.id = %s.parent_id
		JOIN _ast a_outer ON a_outer.node_id = n_outer.id`, prev)

	// Optional qualifier sibling: same parent as the leaf, different kind.
	if wantQualifier && p.QualifierKind != "" {
		sb.WriteString(`
		LEFT JOIN nodes n_pkg ON n_pkg.parent_id = n_leaf.parent_id AND n_pkg.id != n_leaf.id
		LEFT JOIN _ast a_pkg ON a_pkg.node_id = n_pkg.id AND a_pkg.node_kind = ?`)
		args = append(args, p.QualifierKind)
	}

	// WHERE: kind constraints + source_id.
	sb.WriteString(`
		WHERE a_leaf.node_kind = ? AND a_leaf.source_id = ?`)
	args = append(args, p.LeafKind, sourceID)
	for i, anc := range p.Ancestors {
		fmt.Fprintf(&sb, ` AND a_anc%d.node_kind = ?`, i)
		args = append(args, anc)
	}
	sb.WriteString(` AND a_outer.node_kind = ?`)
	args = append(args, p.OuterKind)
	if wantQualifier && p.QualifierKind != "" {
		// Match only rows where the qualifier sibling actually exists.
		sb.WriteString(` AND n_pkg.id IS NOT NULL`)
	}

	rows, err := w.db.Query(sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("call pattern query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []callRow
	for rows.Next() {
		var token, pkg string
		if err := rows.Scan(&token, &pkg); err != nil {
			continue
		}
		out = append(out, callRow{token: token, qualifier: pkg})
	}
	return out, rows.Err()
}
