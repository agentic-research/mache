package cmd

import (
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/agentic-research/mache/internal/lsp"
)

// ensureCanonicalViews installs v_defs / v_refs (ADR-0013 Step 3) as
// TEMP views scoped to the active connection. Probes for ley-line-open's
// post-Step-1 _lsp_* columns (referrer_node_id, ref_token, def_token)
// and includes a UNION ALL with binding-fidelity rows when those
// columns are available; otherwise falls back to mention-only.
//
// Why TEMP views rather than persistent ones:
//
//   - SQLiteGraph opens .dbs read-only (mode=ro). Persistent DDL fails;
//     TEMP objects live in a per-connection in-memory schema and work
//     even on RO connections.
//   - The view body depends on what columns the producer wrote. With
//     TEMP views we can recompute the body per-connection rather than
//     locking in a stale shape on disk.
//   - Temp objects shadow same-named main-schema objects for the
//     current connection, so the legacy persistent mention-only views
//     installed by NewSQLiteWriter (PR #341) get overridden cleanly
//     without a migration.
//
// Idempotent — DROP VIEW IF EXISTS before each CREATE — so safe to
// call before every rule execution.
func ensureCanonicalViews(qg refsQuerier) error {
	// _lsp_defs probe still runs because v_defs's binding-fidelity
	// arm has not yet migrated to capnp — BindingRecord covers refs
	// (the link from a call site to a definition), not standalone
	// def metadata. _lsp_defs migration is tracked separately; for
	// now the def-side dual-read mirrors the pre-mache-6bd4d8 shape.
	hasLSPDefsToken, err := tableHasColumn(qg, "_lsp_defs", "def_token")
	if err != nil {
		return fmt.Errorf("probe _lsp_defs: %w", err)
	}

	// Additive node_hash passthrough (mache-ff9a9d). The merkle-AST
	// producer (LLO) writes an additive node_hash BLOB column onto the
	// occurrence tables (node_defs / node_refs / _ast): identical
	// subtrees dedup to one node_content row keyed by that 32-byte
	// hash, and each OCCURRENCE carries a pointer back to it. mache
	// reads the occurrence layer unchanged (keyed on token / node_id);
	// we surface node_hash as a trailing additive column so downstream
	// consumers can group/annotate by content identity WITHOUT changing
	// the token/node_id keying.
	//
	// INVARIANT: node_hash is ONE-TO-MANY — a deduped subtree appears at
	// many occurrences, so the same node_hash recurs across many
	// (token, node_id) rows. Resolution always targets an occurrence
	// (node_id), NEVER a node_hash; a JOIN on node_hash assuming a
	// single row is a fan-out bug (mirror of be6136).
	//
	// Standalone-mache .db files (written by NewSQLiteWriter, no merkle
	// producer) have no node_hash column. We probe and emit `NULL AS
	// node_hash` in that case — same trailing-column shape on every db,
	// mirroring how `qualifier` is always present in v_refs. Existing
	// consumers select explicit column lists (never SELECT *), so the
	// added trailing column is transparent to them; the rows they read
	// are byte-identical to before.
	hasDefsNodeHash, err := tableHasColumn(qg, "node_defs", "node_hash")
	if err != nil {
		return fmt.Errorf("probe node_defs.node_hash: %w", err)
	}

	// canonical_kind is LLO's closed κ vocabulary (function / method / type /
	// module / constant / ...) written onto node_defs (v0.7.5+, bead
	// ley-line-open-b9d1d5 sibling). It normalizes the per-language tree-sitter
	// kinds (function_declaration / function_item / function_definition → the
	// single canonical 'function') so rules filter by ONE canonical value
	// instead of the mache-schema 'functions/' node_id path (which the leyline
	// projection doesn't use) or an ever-growing per-language kind list. Surface
	// it as a trailing additive column on v_defs; NULL on legacy dbs (and on the
	// LSP binding arm, which has no κ concept) so `canonical_kind = 'function'`
	// filters degrade to "no match" there rather than erroring. (mache-608a3c)
	hasDefsCanonicalKind, err := tableHasColumn(qg, "node_defs", "canonical_kind")
	if err != nil {
		return fmt.Errorf("probe node_defs.canonical_kind: %w", err)
	}
	hasRefsNodeHash, err := tableHasColumn(qg, "node_refs", "node_hash")
	if err != nil {
		return fmt.Errorf("probe node_refs.node_hash: %w", err)
	}

	// referrer_node_id is meant to be the ENCLOSING definition of each ref
	// (its caller), so caller-aggregating rules group by the real caller.
	// The mache-schema/tree-sitter projection puts the enclosing construct in
	// node_refs.node_id directly. The leyline AST-native projection puts the
	// call-SITE path there (a unique leaf per call) and instead emits the
	// enclosing def as a separate additive column, node_refs.container_node_id
	// (ley-line-open v0.7.4+, bead ley-line-open-b9d1d5). Without this, GROUP BY
	// referrer_node_id yields n=1 per call site and fan_out_skew and
	// untested_function's test-caller arm return 0 on leyline. Probe for the
	// column and prefer it; NULLIF falls back to node_id for the ~1% of leyline
	// refs (top-level / file scope) with no enclosing def, and the whole
	// expression degrades to node_id on legacy dbs lacking the column — so
	// tree-sitter/mache-schema behavior is unchanged. (mache-ba3dc6)
	hasRefsContainer, err := tableHasColumn(qg, "node_refs", "container_node_id")
	if err != nil {
		return fmt.Errorf("probe node_refs.container_node_id: %w", err)
	}

	defsHashExpr := "NULL"
	if hasDefsNodeHash {
		defsHashExpr = "node_hash"
	}
	defsCanonExpr := "NULL"
	if hasDefsCanonicalKind {
		defsCanonExpr = "canonical_kind"
	}
	defsBody := `SELECT token, node_id, 'mention' AS fidelity, ` +
		defsHashExpr + ` AS node_hash, ` + defsCanonExpr + ` AS canonical_kind FROM node_defs`
	if hasLSPDefsToken {
		// Binding-fidelity def rows come from _lsp_defs, which has no
		// merkle node_hash or κ canonical_kind concept — emit NULL for
		// both to keep UNION arity.
		defsBody += `
			UNION ALL
			SELECT def_token AS token, node_id, 'binding' AS fidelity,
			       NULL AS node_hash, NULL AS canonical_kind
			FROM _lsp_defs
			WHERE def_token != ''`
	}

	// v_refs columns: referrer_node_id, token, target_node_id,
	// ref_uri, ref_line, fidelity, qualifier. The qualifier column
	// is empty for mention-fidelity rows (tree-sitter call extractor
	// doesn't populate it) and for pre-T8.7 capnp records (default
	// '' per schema-evolution invariant). See mache-6c0d07 for the
	// fan_out_skew metric that consumes it.
	refsHashExpr := "NULL"
	if hasRefsNodeHash {
		refsHashExpr = "node_hash"
	}
	refsReferrerExpr := "node_id"
	if hasRefsContainer {
		refsReferrerExpr = "COALESCE(NULLIF(container_node_id, ''), node_id)"
	}
	refsBody := `SELECT ` + refsReferrerExpr + ` AS referrer_node_id,
	       token,
	       NULL  AS target_node_id,
	       NULL  AS ref_uri,
	       NULL  AS ref_line,
	       'mention' AS fidelity,
	       ''    AS qualifier,
	       ` + refsHashExpr + ` AS node_hash
	FROM node_refs`
	// Binding-fidelity rows come from the per-connection
	// _capnp_binding_refs TEMP table, populated from the sibling
	// .bindings.capnp event log by LoadCapnpBindings (mache-190508).
	// When LoadCapnpBindings hasn't run for this connection, the
	// table stays empty and v_refs surfaces only mention-fidelity
	// rows from node_refs — same as the pre-LSP behavior.
	//
	// The legacy _lsp_refs SQL UNION arm was retired in mache-6bd4d8
	// (T8.8 mirror): SQL columns are no longer the consumer-side
	// contract, the capnp event log is. Removing the arm structurally
	// precludes be6136-class column-name-as-protocol disagreements
	// between LLO writer and mache reader (rather than only
	// preventing them by flag). LLO continues writing _lsp_refs in
	// the transition window; mache no longer reads it.
	// _capnp_binding_refs has no merkle node_hash concept — emit NULL
	// to keep UNION arity with the mention arm's trailing node_hash.
	refsBody += `
		UNION ALL
		SELECT referrer_node_id,
		       token,
		       target_node_id,
		       ref_uri,
		       ref_line,
		       'binding' AS fidelity,
		       qualifier,
		       NULL AS node_hash
		FROM _capnp_binding_refs`

	stmts := []string{
		"DROP VIEW IF EXISTS temp.v_defs",
		"DROP VIEW IF EXISTS temp.v_refs",
		"DROP TABLE IF EXISTS temp._capnp_binding_refs",
		// qualifier column added in mache-6c0d07 (T8.7 mirror).
		// Empty string when LLO didn't see a selector_expression
		// upstream of the ref site, OR when the record came from a
		// pre-T8.7 .bindings.capnp log. fan_out_skew uses
		// COALESCE(NULLIF(qualifier, ''), token) so both shapes
		// degrade gracefully.
		`CREATE TEMP TABLE _capnp_binding_refs (
			referrer_node_id TEXT NOT NULL,
			token TEXT NOT NULL,
			target_node_id TEXT,
			ref_uri TEXT,
			ref_line INTEGER,
			qualifier TEXT NOT NULL DEFAULT ''
		)`,
		"CREATE TEMP VIEW v_defs AS " + defsBody,
		"CREATE TEMP VIEW v_refs AS " + refsBody,
	}
	for _, s := range stmts {
		rows, err := qg.QueryRefs(s)
		if err != nil {
			return fmt.Errorf("ensure canonical views: %w", err)
		}
		_ = rows.Close()
	}
	return nil
}

// LoadCapnpBindings populates the per-connection _capnp_binding_refs
// TEMP table from the sibling .bindings.capnp event log of dbPath.
// Must be called AFTER ensureCanonicalViews on the same connection
// (the TEMP table is created there).
//
// No-op when the sibling log is missing (returns nil) — the canonical
// view's UNION arm just stays empty. Returns an error when the log
// exists but is corrupt.
//
// Producers that write to BOTH _lsp_refs AND .bindings.capnp will
// produce duplicate-shaped rows in v_refs (one per producer); set-
// membership consumers (alive-check in dead_code) deduplicate
// naturally, so this isn't a correctness issue. Once the capnp event
// log is the canonical producer (post-T8.5), the _lsp_refs UNION arm
// in ensureCanonicalViews can be removed.
func LoadCapnpBindings(qg refsQuerier, dbPath string) error {
	if dbPath == "" {
		return nil
	}
	logPath := lsp.SiblingBindingLogPath(dbPath)
	records, err := lsp.ReadBindingLog(logPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load capnp bindings: %w", err)
	}
	if len(records) == 0 {
		return nil
	}

	// Single-row INSERTs are ~30K syscalls on a typical mache-scale
	// log. Acceptable for the first pass; if it shows up in profiles
	// switch to a multi-row INSERT or a prepared-stmt loop.
	for _, r := range records {
		stmt := `INSERT INTO _capnp_binding_refs
			(referrer_node_id, token, target_node_id, ref_uri, ref_line, qualifier)
			VALUES (?, ?, ?, ?, ?, ?)`
		rows, err := qg.QueryRefs(stmt, r.ConstructNodeID, r.RefToken,
			r.TargetNodeID, r.RefURI, int64(r.RefRange.StartLine), r.Qualifier)
		if err != nil {
			return fmt.Errorf("insert capnp binding: %w", err)
		}
		_ = rows.Close()
	}
	return nil
}

// tableHasColumn returns true iff the given table exists AND contains
// a column of the given name. Implemented via PRAGMA table_info, which
// returns zero rows for missing tables (rather than erroring), so this
// helper collapses "table missing" and "column missing" into false.
//
// Used by ensureCanonicalViews to decide whether to add the binding-
// fidelity UNION ALL clause to the v_defs / v_refs body. A pre-Step-1
// _lsp_refs table (without referrer_node_id / ref_token) reads as
// "no binding-fidelity rows available" and the views fall back to
// mention-only — same shape as today.
func tableHasColumn(qg refsQuerier, table, col string) (bool, error) {
	// Table name is interpolated directly; PRAGMA table_info doesn't
	// accept positional parameters. table comes from a hardcoded
	// constant in this file (not user input), so injection risk is
	// nil — but assert anyway via a defensive check.
	if !isSimpleIdent(table) {
		return false, fmt.Errorf("invalid table name: %q", table)
	}
	rows, err := qg.QueryRefs(fmt.Sprintf("PRAGMA table_info(%s)", table))
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
		)
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dfltValue, &pk); err != nil {
			return false, err
		}
		if name == col {
			return true, nil
		}
	}
	return false, rows.Err()
}

// isSimpleIdent guards against SQL injection in PRAGMA table_info
// where the table name can't be parameterized. Allows ASCII letters,
// digits, and underscores — the shape of every table mache writes.
func isSimpleIdent(s string) bool {
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

// dbPathProvider is the opt-in interface a refsQuerier implements
// when it knows its backing .db file path. The path is only used to
// locate the sibling .bindings.capnp event log for capnp-readthrough
// (mache-190508 step 3); queriers that don't know the path (in-memory
// test fixtures, the in-process arena) skip the capnp source
// silently. The mention + legacy SQL binding paths still apply.
type dbPathProvider interface {
	DBPath() string
}
