// Package lloschema is mache's boundary against ley-line-open's physical
// schema.
//
// LLO owns the data plane: it parses source and writes the projection. mache
// is the control plane: it asks questions of what LLO wrote. Under that split
// (ley-line-open-2037b4, and internal/lint/llo_boundary_test.go, which
// enforces the write half) the producer's column names are LLO's to change,
// and every mache query that spells one out is a layering violation waiting
// for a release to expose it.
//
// It was exposed. LLO v0.20.0's projection-v6 renamed `_ast.node_id` to `nid`,
// replaced `_ast.node_kind` with a `kind_id` into `kinds`, dropped `source_id`
// in favour of `nid >> 24`, and replaced `nodes.id`/`name`/`parent_id` with
// `nid`/`name_id`/`parent_nid`. That broke 13 packages, ~136 tests and all
// fourteen smell rules, from four column renames (mache-3688da).
//
// This package translates. It installs TEMP views over whatever the producer
// wrote, giving consumers a STABLE VOCABULARY:
//
//	v_ast    node_id, source_id, node_kind, start_byte … end_col, node_hash
//	v_nodes  node_id, parent_id, name, kind, source_file
//	v_defs   token, node_id, fidelity, node_hash, canonical_kind
//	v_refs   referrer_node_id, node_id, token, target_node_id, …
//
// It is NOT a compatibility shim, and the distinction is the whole design.
// Rendering v6's integer ids back into the old path strings would reinstate
// the ~1 GB of path text across six b-trees that v6 exists to delete. What is
// stable is the vocabulary, not the representation: `node_id` is a path string
// on one producer and an integer on another, and consumers must treat it as
// OPAQUE — equality and joins to another view's `node_id`, never string
// surgery (mache-93e84b isolated the last rule that did otherwise).
//
// Why TEMP views:
//
//   - Graph backends open .dbs read-only (mode=ro), where persistent DDL
//     fails. TEMP objects live in a per-connection in-memory schema and work
//     on a read-only connection.
//   - The body depends on what the producer wrote, so it must be recomputed
//     per connection rather than frozen on disk. A persistent copy is
//     permanently stale — mache shipped one for years, shadowed by these, and
//     removing it is mache-be17ce.
//   - TEMP objects shadow same-named main-schema ones, so a db carrying that
//     old persistent pair still reads correctly.
//
// What does NOT belong here: views derived for a particular CONSUMER's
// question. `v_test_nodes`, `v_vendored_files` and `v_doc_refs` are smell-rule
// helpers built ON TOP of this vocabulary, and they live with the rules.
package lloschema

import (
	"database/sql"
	"fmt"
)

// Querier is the one capability this package needs of a graph backend.
//
// Declared here rather than imported from mache/graph so that `graph` can
// depend on this package without a cycle. graph.RefsQuerier has the identical
// method, so any backend satisfies both without either side knowing.
type Querier interface {
	QueryRefs(query string, args ...any) (*sql.Rows, error)
}

// exec runs a statement for its side effect, closing the rowset the Querier
// interface forces it to return.
func exec(qg Querier, stmt string) error {
	rows, err := qg.QueryRefs(stmt)
	if err != nil {
		return err
	}
	return rows.Close()
}

// TableHasColumn returns true iff the given table exists AND contains
// a column of the given name. Implemented via PRAGMA table_xinfo, which
// returns zero rows for missing tables (rather than erroring), so this
// helper collapses "table missing" and "column missing" into false.
//
// xinfo rather than info because table_info omits GENERATED columns, which
// are readable and therefore usable in a view body — see graph.ColumnExists
// for the full reasoning. xinfo carries one extra trailing column (hidden).
//
// Used by EnsureCanonicalViews to decide whether to add the binding-
// fidelity UNION ALL clause to the v_defs / v_refs body. A pre-Step-1
// _lsp_refs table (without referrer_node_id / ref_token) reads as
// "no binding-fidelity rows available" and the views fall back to
// mention-only — same shape as today.
func TableHasColumn(qg Querier, table, col string) (bool, error) {
	// Table name is interpolated directly; PRAGMA table_xinfo does not
	// accept positional parameters. table comes from a hardcoded
	// constant in this file (not user input), so injection risk is
	// nil — but assert anyway via a defensive check.
	if !IsSimpleIdent(table) {
		return false, fmt.Errorf("invalid table name: %q", table)
	}
	rows, err := qg.QueryRefs(fmt.Sprintf("PRAGMA table_xinfo(%s)", table))
	if err != nil {
		// SQLite returns rows (possibly zero) for valid PRAGMA calls
		// whether or not the table exists; an error here is genuine.
		return false, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			cid       int
			name      string
			typ       string
			notnull   int
			dfltValue sql.NullString
			pk        int
			hidden    int
		)
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dfltValue, &pk, &hidden); err != nil {
			return false, err
		}
		if name == col {
			return true, nil
		}
	}
	return false, rows.Err()
}

// IsSimpleIdent guards against SQL injection in PRAGMA table_xinfo
// where the table name can't be parameterized. Allows ASCII letters,
// digits, and underscores — the shape of every table mache writes.
func IsSimpleIdent(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case 'a' <= r && r <= 'z':
		case 'A' <= r && r <= 'Z':
		case '0' <= r && r <= '9':
		case r == '_':
		default:
			return false
		}
	}
	return true
}
