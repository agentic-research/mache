// Phase 4 of mache-aeb262: chunks-as-parse-outputs.
//
// When the source db has an `_ast` table, mache push emits each
// chunk as a JSON document containing BOTH the source content AND
// the per-source AST node rows. `mache pull` decodes those chunks
// to reconstruct `_source` + `_ast` in the restored db.
//
// When `_ast` is absent (db built via path-only or without an AST
// pass), the v1 fallback applies: chunks = raw source bytes.
// Existing Phase 1+2 tests don't create `_ast`, so they exercise
// the fallback path; the Phase 4 tests in cache_ast_test.go
// explicitly create `_ast` and verify the richer round-trip.
//
// JSON vs capnp: per ADR-0021 §"Topology semantics per consumer,"
// the chunk body is producer-defined. JSON is the simplest correct
// container for v1: human-readable, diff-friendly, no extra schema
// hop. A future bead can migrate chunks to capnp-encoded AstNode
// lists (LLO ast.capnp) once the substrate consumers want byte-
// equal cross-runtime decode.
//
// Wire shape (chunk body):
//
//   {
//     "source_id": "src/auth.go",
//     "path":      "src/auth.go",
//     "language":  "go",
//     "content_b64": "<base64 of raw bytes>",
//     "ast_nodes": [
//       {"node_id": "...", "node_kind": "...", "start_byte": 0, ...},
//       ...
//     ]
//   }
//
// chunk_hash = BLAKE3 of the canonical JSON bytes (sorted keys, no
// trailing whitespace, single newline at EOF). The canonicalization
// is mache-internal — the cache substrate doesn't dictate it.

package cmd

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// astChunkSourceEntry is the wire shape of one chunk body when the
// source db has an `_ast` table. Fields are alphabetically ordered
// (BurntSushi/json's default is field-declaration order, so the
// alphabetization is also the order they appear on disk — pinning
// determinism without a hand-rolled marshaller).
type astChunkSourceEntry struct {
	AstNodes   []astChunkNode `json:"ast_nodes"`
	ContentB64 string         `json:"content_b64"`
	Language   string         `json:"language"`
	Path       string         `json:"path"`
	SourceID   string         `json:"source_id"`
}

// astChunkNode mirrors mache's `_ast` row schema (per
// internal/ingest/ast_walker_test.go). Sufficient to round-trip the
// table verbatim. Omitting line/col cols when zero would save bytes
// but make restoration's defaults harder to reason about; we keep
// them explicit.
type astChunkNode struct {
	NodeID    string `json:"node_id"`
	NodeKind  string `json:"node_kind"`
	StartByte int64  `json:"start_byte"`
	EndByte   int64  `json:"end_byte"`
	StartRow  int64  `json:"start_row"`
	StartCol  int64  `json:"start_col"`
	EndRow    int64  `json:"end_row"`
	EndCol    int64  `json:"end_col"`
}

// dbHasASTTable reports whether the open db has an `_ast` table. The
// presence of the table is the signal that mache push should emit
// Phase 4 (rich) chunks instead of Phase 1 (raw-content) chunks.
//
// Returns (false, nil) when the table doesn't exist (clean Phase 1
// fallback). Returns (false, err) only on a genuine query failure.
func dbHasASTTable(db *sql.DB) (bool, error) {
	var name string
	err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='_ast'").Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check _ast table presence: %w", err)
	}
	return name == "_ast", nil
}

// loadASTNodesForSource returns all `_ast` rows whose source_id
// equals sourceID, in stable node_id order. Used by Phase 4 push to
// populate each chunk's `ast_nodes`.
func loadASTNodesForSource(db *sql.DB, sourceID string) ([]astChunkNode, error) {
	rows, err := db.Query(`
		SELECT node_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col
		FROM _ast
		WHERE source_id = ?
		ORDER BY node_id`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("query _ast for %s: %w", sourceID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []astChunkNode
	for rows.Next() {
		var n astChunkNode
		// Use NullInt64 for the row/col columns since older _ast
		// schemas (pre-bench-test refactor) omit them. NULL → 0,
		// which round-trips identically.
		var startRow, startCol, endRow, endCol sql.NullInt64
		if err := rows.Scan(&n.NodeID, &n.NodeKind, &n.StartByte, &n.EndByte,
			&startRow, &startCol, &endRow, &endCol); err != nil {
			return nil, fmt.Errorf("scan _ast for %s: %w", sourceID, err)
		}
		n.StartRow = startRow.Int64
		n.StartCol = startCol.Int64
		n.EndRow = endRow.Int64
		n.EndCol = endCol.Int64
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows.Err for %s: %w", sourceID, err)
	}
	return out, nil
}

// encodeASTChunk renders one source's full Phase 4 chunk body. The
// resulting bytes are what gets BLAKE3'd to produce chunk_hash and
// what gets written into <out>/objects/<hash[0..2]>/<hash[2..]>.
//
// Determinism: json.Encoder + struct-field declaration order +
// trailing newline. Two calls with identical inputs MUST produce
// identical bytes.
func encodeASTChunk(src sourceRow, nodes []astChunkNode) ([]byte, error) {
	entry := astChunkSourceEntry{
		AstNodes:   nodes,
		ContentB64: base64.StdEncoding.EncodeToString(src.content),
		Language:   src.language,
		Path:       src.path,
		SourceID:   src.id,
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(&entry); err != nil {
		return nil, fmt.Errorf("encode AST chunk for %s: %w", src.id, err)
	}
	// json.Encoder.Encode adds a single trailing newline already.
	return buf.Bytes(), nil
}

// decodeASTChunk is the inverse: parse a Phase 4 chunk body and
// surface its fields for the pull-side INSERT INTO _source/_ast.
func decodeASTChunk(body []byte) (astChunkSourceEntry, error) {
	var entry astChunkSourceEntry
	if err := json.Unmarshal(body, &entry); err != nil {
		return entry, fmt.Errorf("decode AST chunk: %w", err)
	}
	if entry.SourceID == "" {
		return entry, errors.New("AST chunk missing source_id")
	}
	return entry, nil
}

// chunkBodyIsASTShape returns true if the bytes parse as a Phase 4
// JSON chunk (well-formed JSON with a source_id field). False
// without error for any other shape — the caller treats it as the
// Phase 1 raw-bytes fallback.
//
// Cheap: doesn't fully decode; just looks for the top-level marker.
func chunkBodyIsASTShape(body []byte) bool {
	if len(body) == 0 || body[0] != '{' {
		return false
	}
	// Avoid full json.Unmarshal here — we just want a fast check.
	var probe struct {
		SourceID string `json:"source_id"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	return probe.SourceID != ""
}

// restoreFromASTChunk inserts a Phase 4 chunk's content back into
// `_source` AND `_ast`. The caller passes a prepared INSERT for
// `_source` and `_ast` so the txn is shared across all chunks.
func restoreFromASTChunk(entry astChunkSourceEntry, sourceStmt, astStmt *sql.Stmt) error {
	content, err := base64.StdEncoding.DecodeString(entry.ContentB64)
	if err != nil {
		return fmt.Errorf("decode content_b64 for %s: %w", entry.SourceID, err)
	}
	if _, err := sourceStmt.Exec(entry.SourceID, entry.Path, entry.Language, content); err != nil {
		return fmt.Errorf("insert _source for %s: %w", entry.SourceID, err)
	}
	for _, n := range entry.AstNodes {
		if _, err := astStmt.Exec(n.NodeID, entry.SourceID, n.NodeKind,
			n.StartByte, n.EndByte, n.StartRow, n.StartCol, n.EndRow, n.EndCol); err != nil {
			return fmt.Errorf("insert _ast for %s/%s: %w", entry.SourceID, n.NodeID, err)
		}
	}
	return nil
}
