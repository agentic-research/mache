package graph

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// Resolution says WHY a reference did or did not reach a definition. It exists
// because "no node id" has three different meanings for a consumer walking a
// blast radius, and collapsing them is how a tool reports the wrong radius with
// full confidence.
//
// Measured on this repo's own projection (123,117 refs), the split is not
// close: 10.8% resolve uniquely, 13.2% are external, 17.8% are genuinely
// ambiguous, and 58.1% have no local definition at all. A boolean "resolved?"
// would put 89% of references in one undifferentiated bucket.
type RefResolution string

const (
	// RefResolved: exactly one definition in this projection. Follow it.
	RefResolved RefResolution = "resolved"
	// RefExternal: the reference is qualified by an import that leaves this
	// projection (stdlib, a third-party module). There is deliberately no
	// local target, and the local blast radius is genuinely zero — this is an
	// ANSWER, not a failure. Matching such a token against a same-named local
	// definition is the bug this value exists to prevent.
	RefExternal RefResolution = "external"
	// RefAmbiguous: several definitions share the token and nothing available
	// narrows it. Candidates are returned; the caller decides, because only
	// the caller knows whether a guess is acceptable.
	RefAmbiguous RefResolution = "ambiguous"
	// RefNoTarget: no definition anywhere in the projection. Language
	// builtins, locals, field names — node_refs records every identifier, not
	// only calls to definable symbols.
	RefNoTarget RefResolution = "no-target"
)

// RefTarget is where a reference points. NodeID is set ONLY for RefResolved:
// a caller cannot accidentally consume a guess, because an unresolved target
// has nothing to consume.
type RefTarget struct {
	Resolution RefResolution
	// NodeID is the single definition, empty unless Resolution is RefResolved.
	NodeID string
	// Candidates lists the competing definitions when RefAmbiguous.
	Candidates []string
	// Via names the import path when RefExternal — the module the call leaves
	// through, which is what a dependency-aware consumer wants to record.
	Via string
}

// RefRange is a reference's span in its source file. Lines and columns are
// 1-BASED, matching what an editor shows; the underlying `_ast` rows are
// tree-sitter's 0-based values and are converted here so no consumer has to
// remember which convention it is holding.
type RefRange struct {
	SourceID  string
	StartLine uint32
	StartCol  uint32
	EndLine   uint32
	EndCol    uint32
	StartByte uint32
	EndByte   uint32
}

// RefRangeOf returns where the reference node sits in its file.
//
// Every consumer previously hand-wrote this join (node_refs.node_id -> _ast),
// which is why it is here: the join is not the interesting part of anyone's
// program. Returns nil when the node has no `_ast` row — a standalone mache
// projection carries no spans, and nil is the honest answer rather than a
// zero-valued range that reads as "line 0".
func (g *SQLiteGraph) RefRangeOf(nodeID string) (*RefRange, error) {
	if !ColumnExists(g.db, "_ast", "node_id") {
		return nil, nil
	}
	row := g.db.QueryRow(
		`SELECT source_id, start_byte, end_byte, start_row, start_col, end_row, end_col
		   FROM _ast WHERE node_id = ?`, nodeID)

	var (
		src                                string
		sb, eb                             int64
		startRow, startCol, endRow, endCol sql.NullInt64
	)
	switch err := row.Scan(&src, &sb, &eb, &startRow, &startCol, &endRow, &endCol); {
	case err == sql.ErrNoRows:
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("ref range for %s: %w", nodeID, err)
	}
	return &RefRange{
		SourceID:  src,
		StartByte: uint32(sb),
		EndByte:   uint32(eb),
		StartLine: oneBased(startRow),
		StartCol:  oneBased(startCol),
		EndLine:   oneBased(endRow),
		EndCol:    oneBased(endCol),
	}, nil
}

// oneBased converts a tree-sitter 0-based row/column to the 1-based value
// editors display. A NULL stays 0, which is distinguishable from line 1.
func oneBased(v sql.NullInt64) uint32 {
	if !v.Valid {
		return 0
	}
	return uint32(v.Int64 + 1)
}

// ResolveRef answers "what definition does this reference point at?" for the
// node id of a reference site.
//
// It replaces bare-token matching against node_refs.token, which cannot
// distinguish a local `Join` from `filepath.Join` and will happily return the
// wrong one. The classification is derived, never guessed:
//
//	no local def + a known import  -> RefExternal (Via names the import)
//	no local def, no import        -> RefNoTarget
//	exactly one local def          -> RefResolved
//	several, narrowed to one       -> RefResolved
//	several, still several         -> RefAmbiguous (candidates returned)
//
// Note what is NOT here: no attempt to decide whether an import path "looks
// external" by inspecting its shape. Whether github.com/you/yours is inside
// your module is something the caller knows and mache does not, so mache
// reports the import path and lets the caller judge.
func (g *SQLiteGraph) ResolveRef(nodeID string) (*RefTarget, error) {
	// A node_id is NOT a unique key into node_refs: a qualified call dual-emits
	// two rows at the same node — `require.NoError` and `NoError`. 37,236 node
	// ids in this repo's own projection carry more than one. Picking
	// arbitrarily is a correctness bug, because node_defs is keyed on the BARE
	// token, so landing on the qualified row reports a false "no target".
	//
	// LLO's table contract makes the choice deterministic: qualifier is set on
	// the bare-token row of the pair and NULL on the qualified-token row. So
	// ordering a non-NULL qualifier first selects the row whose token can
	// actually match a definition, and falls through to the single row for an
	// unqualified call.
	var token, qualifier, sourceID string
	err := g.db.QueryRow(
		`SELECT token, COALESCE(qualifier,''), source_id FROM node_refs
		  WHERE node_id = ?
		  ORDER BY (qualifier IS NOT NULL AND qualifier != '') DESC, token
		  LIMIT 1`,
		nodeID).Scan(&token, &qualifier, &sourceID)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("no reference at node %s", nodeID)
	}
	if err != nil {
		return nil, fmt.Errorf("load reference %s: %w", nodeID, err)
	}

	importPath := g.importFor(qualifier, sourceID)

	defs, err := g.defsForToken(token)
	if err != nil {
		return nil, err
	}

	switch {
	case len(defs) == 0 && importPath != "":
		return &RefTarget{Resolution: RefExternal, Via: importPath}, nil
	case len(defs) == 0:
		return &RefTarget{Resolution: RefNoTarget}, nil
	case len(defs) == 1:
		return &RefTarget{Resolution: RefResolved, NodeID: defs[0], Via: importPath}, nil
	}

	if narrowed := narrowToPackage(defs, importPath); len(narrowed) == 1 {
		return &RefTarget{Resolution: RefResolved, NodeID: narrowed[0], Via: importPath}, nil
	}
	return &RefTarget{Resolution: RefAmbiguous, Candidates: defs, Via: importPath}, nil
}

// importFor returns the import path a qualifier names in this file, or "".
//
// A qualifier is receiver-or-selector text, so it is a package alias for
// `filepath.Join` and a local variable for `t.TempDir`. Only the first kind
// matches an import, which is exactly the discrimination wanted: a variable
// receiver yields "" and the caller learns nothing false.
func (g *SQLiteGraph) importFor(qualifier, sourceID string) string {
	if qualifier == "" || !ColumnExists(g.db, "_imports", "alias") {
		return ""
	}
	var path string
	err := g.db.QueryRow(
		`SELECT path FROM _imports
		  WHERE source_id = ? AND (alias = ? OR path LIKE '%/' || ?)
		  LIMIT 1`, sourceID, qualifier, qualifier).Scan(&path)
	if err != nil {
		return ""
	}
	return path
}

// defsForToken returns every definition node for token, sorted for determinism
// so an ambiguous result is stable across runs — an unstable candidate list
// makes a diffable report churn for no reason.
func (g *SQLiteGraph) defsForToken(token string) ([]string, error) {
	rows, err := g.db.Query(
		`SELECT DISTINCT node_id FROM node_defs WHERE token = ? ORDER BY node_id`, token)
	if err != nil {
		return nil, fmt.Errorf("defs for %s: %w", token, err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// narrowToPackage keeps only the definitions whose file sits under the
// directory the import path ends with. It is a filter, never a tiebreak: if it
// does not reduce to exactly one, the caller still gets RefAmbiguous rather
// than a plausible-looking pick.
func narrowToPackage(defs []string, importPath string) []string {
	if importPath == "" {
		return defs
	}
	dir := importPath
	if i := strings.LastIndexByte(importPath, '/'); i >= 0 {
		dir = importPath[i+1:]
	}
	if dir == "" {
		return defs
	}
	var kept []string
	for _, d := range defs {
		if hasPathSegment(d, dir) {
			kept = append(kept, d)
		}
	}
	if len(kept) == 0 {
		return defs
	}
	return kept
}

// hasPathSegment reports whether id contains dir as a WHOLE path segment, so
// "graph" matches "graph/graph.go" but not "subgraph/x.go". Substring matching
// would attribute definitions to the wrong package.
func hasPathSegment(id, dir string) bool {
	return slices.Contains(strings.Split(id, "/"), dir)
}

// Compile-time proof that the SQL backend satisfies the capability, so a
// signature drift breaks the build here rather than at a consumer's type
// assertion, which would silently degrade to "capability absent".
var (
	_ RefResolver      = (*SQLiteGraph)(nil)
	_ DefsNodeLookuper = (*SQLiteGraph)(nil)
)

// LookupDefNodes returns the definition NODES for token, closing the return-type
// asymmetry that made every consumer write the same conversion loop:
//
//	LookupDef(token)  ->  []string          // ids
//	GetCallers(token) ->  ([]*Node, error)  // nodes
//	GetCallees(id)    ->  ([]*Node, error)  // nodes
//
// Ids from one accessor and Nodes from the next is why modmap wrapped this
// surface in its own callerResolver. LookupDef stays as-is — ids ARE the right
// answer when the caller only needs identity, and fetching nodes it will throw
// away is waste. This is the variant for when it actually wants the node.
//
// A definition whose node cannot be loaded is SKIPPED rather than failing the
// call: node_defs can outlive a node during an incremental reparse, and one
// stale row should not deny a caller the definitions that did resolve.
func (g *SQLiteGraph) LookupDefNodes(token string) ([]*Node, error) {
	ids, err := g.defsForToken(token)
	if err != nil {
		return nil, err
	}
	out := make([]*Node, 0, len(ids))
	for _, id := range ids {
		n, err := g.GetNode(id)
		if err != nil {
			continue // stale index row; the other definitions still stand
		}
		out = append(out, n)
	}
	return out, nil
}
