package fixturedb

import (
	"database/sql"
	"encoding/hex"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/zeebo/blake3"
)

// Emission: the ONE place a spec becomes columns.
//
// Every producer difference lives in the producer's [dialect], never in a
// caller and never in a `CREATE TABLE` a test wrote. That is what makes "which
// producer did this test mean?" a question with an answer. This file holds the
// row writers the dialects share.

// subtree records a deduped merkle subtree in node_content and returns the hash
// occurrences point at. Returns nil when the fixture has no node_content table,
// so node_hash lands NULL rather than dangling.
func (e *emitter) subtree(label, kind, token string) []byte {
	if !e.hasNodeContent {
		return nil
	}
	h := subtreeHash(label)
	e.exec(`INSERT OR IGNORE INTO node_content (node_hash, node_tag, kind, raw_kind, lang, token, arity)
		VALUES (?, 0, ?, ?, '', ?, 0)`, h, kind, kind, nullIfEmpty(token))
	return h
}

// insertV4Nodes writes one projection-v4 `nodes` row per construct.
func (e *emitter) insertV4Nodes() {
	for _, id := range e.b.order {
		c := e.b.constructs[id]
		kind := 1
		if c.leaf {
			kind = 0
		}
		e.insertV4Node(string(c.id), string(c.parent), c.name, kind, c.record, string(c.source))
	}
}

// insertV4Node writes one projection-v4 `nodes` row, leaving parent_id to
// SQLite when the table generates it.
func (e *emitter) insertV4Node(id, parent, name string, kind int, record any, source string) {
	if e.derivedParent {
		e.exec(`INSERT OR REPLACE INTO nodes (id, name, kind, size, mtime, record_id, record, source_file)
			VALUES (?, ?, ?, 0, 0, '', ?, ?)`, id, name, kind, record, source)
		return
	}
	e.exec(`INSERT OR REPLACE INTO nodes (id, parent_id, name, kind, size, mtime, record_id, record, source_file)
		VALUES (?, ?, ?, ?, 0, 0, '', ?, ?)`, id, parent, name, kind, record, source)
}

// insertV4AST writes the projection-v4 `_ast` rows and returns each node's
// hash, keyed by node id, for the child lists.
func (e *emitter) insertV4AST() map[string][]byte {
	hashes := make(map[string][]byte, len(e.b.ast))
	for _, a := range e.b.ast {
		h := e.subtree(a.subtree, a.kind, a.token)
		hashes[a.nodeID] = h
		e.exec(`INSERT OR REPLACE INTO _ast
			(node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col, node_hash)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			a.nodeID, string(a.source), a.kind,
			a.span.StartByte, a.span.EndByte,
			a.span.StartRow, a.span.StartCol, a.span.EndRow, a.span.EndCol, h)
	}
	return hashes
}

// insertV6AST writes the projection-v6 `_ast` rows, keyed by nid, and returns
// each node's hash, keyed by node id, for the child lists.
func (e *emitter) insertV6AST() map[string][]byte {
	t := e.v6Tree()
	hashes := make(map[string][]byte, len(e.b.ast))
	for _, a := range e.b.ast {
		h := e.subtree(a.subtree, a.kind, a.token)
		hashes[a.nodeID] = h
		e.exec(`INSERT OR REPLACE INTO _ast
			(nid, kind_id, start_byte, end_byte, start_row, start_col, end_row, end_col, content_id)
			VALUES (?, (SELECT kind_id FROM kinds WHERE raw_kind = ? AND lang = ?), ?, ?, ?, ?, ?, ?, (SELECT content_id FROM node_content WHERE node_hash = ?))`,
			t.byPath[a.nodeID].nid, a.kind, e.b.langOf(a.source),
			a.span.StartByte, a.span.EndByte, a.span.StartRow, a.span.StartCol, a.span.EndRow, a.span.EndCol, h)
	}
	return hashes
}

// The merkle edge writers, one per producer table, each taking (parent hash,
// ordinal, child hash, field). projection-v9 keeps the edges in node_edge,
// numbering each end by its node_content row, and makes node_child a view a
// write to fails; before v9 node_child is the table.
const (
	insertNodeChild = `INSERT INTO node_child (parent_hash, ordinal, child_hash, field) VALUES (?, ?, ?, ?)`
	insertNodeEdge  = `INSERT INTO node_edge (parent_id, ordinal, child_id, field) VALUES (
		(SELECT content_id FROM node_content WHERE node_hash = ?), ?,
		(SELECT content_id FROM node_content WHERE node_hash = ?), ?)`
)

// emitNodeChildren writes the merkle child list: for every `_ast` node whose
// id's directory is itself an `_ast` node, one row under the parent's hash at
// the child's start-byte position among its siblings. ley-line emits a parent's
// list once per unique hash, so two ASTNodes sharing a [Detail.Subtree] label
// share one list — and must therefore agree on it: identical content has
// identical children, so a fixture whose labelled twins disagree is describing
// a subtree that cannot exist.
//
// The producer's dialect names the edge writer: the table differs, the list
// does not.
func (e *emitter) emitNodeChildren(hashes map[string][]byte) {
	insert := e.b.producer.dialect.edgeInsert()
	byParent := make(map[string][]astSpec)
	for _, a := range e.b.ast {
		parent := path.Dir(a.nodeID)
		if _, ok := hashes[parent]; ok {
			byParent[parent] = append(byParent[parent], a)
		}
	}
	listed := make(map[string][]childRow)
	for _, a := range e.b.ast {
		kids, ok := byParent[a.nodeID]
		if !ok {
			continue
		}
		slices.SortStableFunc(kids, func(x, y astSpec) int { return x.span.StartByte - y.span.StartByte })
		rows := make([]childRow, len(kids))
		for i, k := range kids {
			rows[i] = childRow{hash: hashes[k.nodeID], field: k.field}
		}
		key := hex.EncodeToString(hashes[a.nodeID])
		if prior, seen := listed[key]; seen {
			if !sameChildList(prior, rows) {
				e.b.t.Fatalf("fixturedb: %s shares a Subtree label with an earlier ASTNode "+
					"but declares different children — one subtree has one child list", a.nodeID)
			}
			continue
		}
		listed[key] = rows
		for ord, r := range rows {
			e.exec(insert, hashes[a.nodeID], ord, r.hash, nullIfEmpty(r.field))
		}
	}
}

// emitLSP writes the LSP-enrichment rows: _lsp_defs, _lsp and _lsp_hover.
func (e *emitter) emitLSP() {
	_, v6 := e.b.producer.dialect.(leylineV6Dialect)
	// key is the column value naming a node: its nid on v6, which the spec
	// must declare, its id on v4.
	key := func(table, id string) any {
		if !v6 {
			return id
		}
		n, ok := e.v6Tree().byPath[id]
		if !ok {
			e.b.t.Fatalf("fixturedb(leyline-v6): %s row on %q, which the spec never declares; "+
				"v6 keys %s by the node's nid, so the node must exist", table, id, table)
		}
		return n.nid
	}
	col := "node_id"
	if v6 {
		col = "nid"
	}
	for _, d := range e.b.lspDefs {
		e.exec(`INSERT INTO _lsp_defs
			(`+col+`, def_token, def_uri, def_start_line, def_start_col, def_end_line, def_end_col)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			key("_lsp_defs", d.nodeID), d.token, d.uri, d.startLine, d.startCol, d.endLine, d.endCol)
	}
	for _, l := range e.b.lspSyms {
		e.exec(`INSERT INTO _lsp (`+col+`, symbol_kind, detail, start_line, start_col, end_line, end_col, diagnostics)
			VALUES (?, ?, '', 0, 0, 0, 0, ?)`, key("_lsp", l.nodeID), l.symbolKind, nullIfEmpty(l.diagnostics))
	}
	for _, h := range e.b.lspHovers {
		if !v6 {
			e.insertV4Chain(h.symbol)
		}
		e.exec(`INSERT OR REPLACE INTO _lsp_hover (`+col+`, hover_text) VALUES (?, ?)`, key("_lsp_hover", h.symbol), h.text)
	}
}

// insertV4Chain writes a standalone address's nodes rows on projection-v4, one
// directory per link, as LLO's standalone_chain does. v6 interns the same chain
// as directories when it builds the tree.
func (e *emitter) insertV4Chain(p string) {
	parent := ""
	for _, seg := range strings.Split(p, "/") {
		id := seg
		if parent != "" {
			id = parent + "/" + seg
		}
		e.insertV4Node(id, parent, seg, 1, "", "")
		parent = id
	}
}

// insertV4Sources writes the projection-v4 `_source` rows. leyline's own
// (blobs) carry their bytes as leyline does, in source_blobs under
// content_hash with content NULL; mache's cache-hydrated db (Standalone)
// carries them inline, as buildcache writes them.
func (e *emitter) insertV4Sources(blobs bool) {
	for _, id := range e.b.srcOrder {
		s := e.b.sources[id]
		body, hash := e.sourceBytes(s, blobs)
		e.exec(`INSERT OR REPLACE INTO _source (id, language, content, path, content_hash)
			VALUES (?, ?, ?, ?, ?)`, string(s.id), s.lang, body, s.path, hash)
	}
}

// sourceBytes is what a `_source` row carries for s: inline content, or (blobs)
// NULL content and the hash of a source_blobs row holding the bytes. A source
// declared without content carries neither.
func (e *emitter) sourceBytes(s *sourceSpec, blobs bool) (body, hash any) {
	if s.content == "" {
		return nil, nil
	}
	if !blobs {
		return []byte(s.content), nil
	}
	return nil, storeBlob(e.exec, s.content)
}

// insertV6Sources writes the projection-v6 `_source` rows, each with the
// interned file id its nodes are keyed under.
func (e *emitter) insertV6Sources() {
	t := e.v6Tree()
	for _, id := range e.b.srcOrder {
		s := e.b.sources[id]
		body, hash := e.sourceBytes(s, true)
		e.exec(`INSERT OR REPLACE INTO _source (id, language, content, path, content_hash, file_id)
			VALUES (?, ?, ?, ?, ?, ?)`, string(s.id), s.lang, body, s.path, hash, t.byPath[string(s.id)].fileID)
	}
}

// storeBlob writes content's source_blobs row through exec and returns its
// hash: BLAKE3 of the bytes, as leyline keys source_blobs and _tree.blob_hash
// (checked against a real parse; ADR-0047 leaves it the one unkeyed
// construction). A fixture blob carries the hash a real arena would, so code
// that compares or verifies blob hashes sees the same thing on both
// (mache-10a306).
func storeBlob(exec func(string, ...any), content string) []byte {
	h := blake3.Sum256([]byte(content))
	exec(`INSERT OR IGNORE INTO source_blobs (blob_hash, blob_bytes) VALUES (?, ?)`, h[:], []byte(content))
	return h[:]
}

// Damage to a stored blob, for tests of mache's read-time verification
// (mache-c1eb7a). The writes live here, beside storeBlob, because the LLO
// boundary lint lets only this file write leyline's tables; they take a
// *sql.DB so a real leyline parse can be damaged the same way as a fixture.

// DamageBlob damages the row stored under hash in the way d names.
func DamageBlob(t testing.TB, db *sql.DB, hash []byte, d BlobDamage) {
	t.Helper()
	switch d {
	case BlobTampered:
		mustExec(t, db, `UPDATE source_blobs SET blob_bytes = CAST('tampered' AS BLOB) WHERE blob_hash = ?`, hash)
	case BlobChunked:
		// leyline's DDL declares blob_bytes NOT NULL; a chunk-activated store
		// relaxes it, so the table is rebuilt without the constraint first.
		for _, stmt := range []string{
			`CREATE TABLE source_blobs_chunked (blob_hash BLOB PRIMARY KEY, blob_bytes BLOB)`,
			`INSERT INTO source_blobs_chunked SELECT blob_hash, blob_bytes FROM source_blobs`,
			`DROP TABLE source_blobs`,
			`ALTER TABLE source_blobs_chunked RENAME TO source_blobs`,
		} {
			mustExec(t, db, stmt)
		}
		mustExec(t, db, `UPDATE source_blobs SET blob_bytes = NULL WHERE blob_hash = ?`, hash)
	case BlobDropped:
		mustExec(t, db, `DELETE FROM source_blobs WHERE blob_hash = ?`, hash)
	default:
		t.Fatalf("fixturedb: unknown BlobDamage %d", d)
	}
}

// AddBlob stores content under its BLAKE3 hash, as leyline keys it, and
// returns the hash.
func AddBlob(t testing.TB, db *sql.DB, content []byte) []byte {
	t.Helper()
	return storeBlob(func(q string, args ...any) { mustExec(t, db, q, args...) }, string(content))
}
