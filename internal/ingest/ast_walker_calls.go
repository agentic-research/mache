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
	// RequirePriorSibling, when true, restricts the match to leaves whose
	// immediate ancestor (Ancestors[0], the direct child of OuterKind) has
	// an EARLIER same-kind sibling under OuterKind. This reproduces
	// tree-sitter's positional matching for value-position captures like
	//   (keyed_element (literal_element) (literal_element (identifier) @x))
	// where only the SECOND literal_element (the value) is captured, not
	// the first (the key). Requires len(Ancestors) >= 1.
	RequirePriorSibling bool
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
		rows, err := w.queryCallPattern(sourceID, p, false, "")
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

// ExtractCallsScoped is the per-construct equivalent of ExtractCalls: it
// returns only call tokens whose leaf node lives under scopeID's id-path —
// matching SitterWalker's per-scope ExtractCalls(scopeNode, ...). sourceID is
// the _source key (filepath.Base of the file). Empty scopeID would match the
// whole file; callers pass a real construct id.
func (w *ASTWalker) ExtractCallsScoped(sourceID, scopeID, langName string) ([]string, error) {
	raw, ok := callPatternRegistry.Load(langName)
	if !ok {
		return nil, nil
	}
	patterns := raw.([]CallPattern)
	if len(patterns) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool)
	var calls []string
	for _, p := range patterns {
		rows, err := w.queryCallPattern(sourceID, p, false, scopeID)
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

// fileLevelRefPatternRegistry stores per-language CallPatterns used by
// ExtractFileLevelRefs — the file-wide identifier-capture shapes that
// per-scope ExtractCalls can't see (e.g. Go top-level cobra var func
// values, mache-02r9). Mirrors SitterWalker's file-level ref query, but
// as structured kind-chains queried over _ast/nodes.
var fileLevelRefPatternRegistry sync.Map // langName → []CallPattern

// RegisterASTFileLevelRefPatterns registers the per-language file-level
// ref capture shapes for ExtractFileLevelRefs.
func RegisterASTFileLevelRefPatterns(langName string, patterns []CallPattern) {
	fileLevelRefPatternRegistry.Store(langName, patterns)
}

// ExtractFileLevelRefs returns deduplicated identifier tokens captured by
// the language's file-level ref patterns across the WHOLE file. Mirrors
// SitterWalker.ExtractFileLevelRefs (the _file_level: sentinel feed that
// dead_code reads) but queries _ast/nodes via SQL instead of CGO
// tree-sitter. sourceID is the _source key (filepath.Base of the file).
// Returns (nil, nil) when no patterns are registered for the language.
func (w *ASTWalker) ExtractFileLevelRefs(sourceID, langName string) ([]string, error) {
	raw, ok := fileLevelRefPatternRegistry.Load(langName)
	if !ok {
		return nil, nil
	}
	patterns := raw.([]CallPattern)
	if len(patterns) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool)
	var refs []string
	for _, p := range patterns {
		// Unlike the generic Query path (where a selector may legitimately
		// be unsupported), queryCallPattern generates SQL from a structured
		// kind-chain — an error here is a real bug (invalid pattern or a
		// failed query), not an unsupported pattern. Surface it rather than
		// returning a silently-incomplete ref set: these tokens feed the
		// _file_level: sentinel dead_code reads, so a short set with nil
		// error would manifest as silent dead_code false positives.
		rows, err := w.queryCallPattern(sourceID, p, false, "")
		if err != nil {
			return nil, fmt.Errorf("file-level ref pattern %s/%s: %w", p.OuterKind, p.LeafKind, err)
		}
		for _, r := range rows {
			if r.token != "" && !seen[r.token] {
				seen[r.token] = true
				refs = append(refs, r.token)
			}
		}
	}
	return refs, nil
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
		rows, err := w.queryCallPattern(sourceID, p, true, "")
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
func (w *ASTWalker) queryCallPattern(sourceID string, p CallPattern, wantQualifier bool, scopePrefix string) ([]callRow, error) {
	if p.OuterKind == "" || p.LeafKind == "" {
		return nil, fmt.Errorf("invalid CallPattern: OuterKind and LeafKind required")
	}
	// RequirePriorSibling references the immediate-ancestor alias (a_anc0),
	// so it's meaningless without at least one ancestor. Reject rather than
	// silently dropping the constraint, which would over-capture undetectably.
	if p.RequirePriorSibling && len(p.Ancestors) == 0 {
		return nil, fmt.Errorf("invalid CallPattern: RequirePriorSibling requires at least one ancestor")
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
	// Scope to a single construct subtree (calls whose leaf node lives under the
	// scope node's id path). Empty scopePrefix = whole file.
	if scopePrefix != "" {
		sb.WriteString(` AND n_leaf.id LIKE ?`)
		args = append(args, scopePrefix+"/%")
	}
	for i, anc := range p.Ancestors {
		fmt.Fprintf(&sb, ` AND a_anc%d.node_kind = ?`, i)
		args = append(args, anc)
	}
	sb.WriteString(` AND a_outer.node_kind = ?`)
	args = append(args, p.OuterKind)
	// Value-position constraint: require an earlier same-kind sibling of
	// the immediate ancestor under the outer node (e.g. the key
	// literal_element preceding the value literal_element in a
	// keyed_element). Reproduces tree-sitter positional capture.
	// len(Ancestors) >= 1 is guaranteed by the validation above.
	if p.RequirePriorSibling {
		sb.WriteString(`
			AND EXISTS (
				SELECT 1 FROM nodes sib
				JOIN _ast a_sib ON a_sib.node_id = sib.id
				WHERE sib.parent_id = n_outer.id
				  AND a_sib.node_kind = ?
				  AND a_sib.start_byte < a_anc0.start_byte
			)`)
		args = append(args, p.Ancestors[0])
	}
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
