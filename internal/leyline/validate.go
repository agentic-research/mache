package leyline

// Client for the daemon's `validate` op (ley-line-open >= v0.7.8).
//
// Request wire (rs/ll-open/cli-lib/src/daemon/wire.rs ValidateRequest):
//
//	{"op":"validate", "content":"<utf8 source>", "language":"go",
//	 "path":"main.go", "emit_ast":true}
//
// `language` is an extension key (go|py|js|ts|tsx|rs|ex|exs); `path` lets the
// daemon infer the language from the extension. When both are present,
// `language` wins. When `emit_ast` is true, `path` doubles as the source_id
// stamped on every emitted AST row.
//
// Response wire (rs/ll-open/cli-lib/src/daemon/ops.rs op_validate):
//
//	{"ok": bool,
//	 "errors": [{"row":N,"col":N,"byte_start":N,"byte_end":N,"message":"..."}],
//	 "diagnostics": [legacy first-error-only shape — ignored here],
//	 "ast": {...}}   // only when emit_ast was requested
//
// `errors` enumerates EVERY ERROR/MISSING node in document order; row/col are
// 0-based (col = byte offset within the row), byte_start == byte_end for
// zero-width MISSING nodes, message is "syntax error" or "missing <kind>".
// Daemons older than v0.7.7 lack the `errors` key entirely — that is treated
// as a hard "daemon too old" error, never a silent fallback (the mache binary
// pin guarantees v0.7.8 for daemons mache spawns, but LEYLINE_SOCKET may point
// at a user-supplied older daemon).

import (
	"fmt"
)

// ValidateSyntaxError is one ERROR/MISSING node from the daemon's validate
// op. Row and Col are 0-based; Col is the byte offset within the row.
// ByteStart == ByteEnd for zero-width MISSING nodes.
type ValidateSyntaxError struct {
	Row       uint32 `json:"row"`
	Col       uint32 `json:"col"`
	ByteStart uint32 `json:"byte_start"`
	ByteEnd   uint32 `json:"byte_end"`
	Message   string `json:"message"`
}

// ASTRow mirrors one `_ast` table row emitted by the daemon's emit_ast
// extension. NodeID is the hierarchical merkle-AST path (parent node_id +
// "/" + child kind, with a `_N` index suffix when a parent has multiple
// same-kind named children), rooted at the request's source_id.
type ASTRow struct {
	NodeID    string `json:"node_id"`
	SourceID  string `json:"source_id"`
	NodeKind  string `json:"node_kind"`
	StartByte uint32 `json:"start_byte"`
	EndByte   uint32 `json:"end_byte"`
	StartRow  uint32 `json:"start_row"`
	StartCol  uint32 `json:"start_col"`
	EndRow    uint32 `json:"end_row"`
	EndCol    uint32 `json:"end_col"`
	NodeHash  string `json:"node_hash"`
}

// ASTDef mirrors one `node_defs` row from emit_ast.
type ASTDef struct {
	Token           string  `json:"token"`
	NodeID          string  `json:"node_id"`
	SourceID        string  `json:"source_id"`
	ContainerNodeID *string `json:"container_node_id"`
	CanonicalKind   *string `json:"canonical_kind"`
}

// ASTRef mirrors one `node_refs` row from emit_ast.
type ASTRef struct {
	Token           string  `json:"token"`
	NodeID          string  `json:"node_id"`
	SourceID        string  `json:"source_id"`
	ContainerNodeID *string `json:"container_node_id"`
}

// ASTImport mirrors one `_imports` row from emit_ast.
type ASTImport struct {
	Alias    string `json:"alias"`
	Path     string `json:"path"`
	SourceID string `json:"source_id"`
}

// ASTPayload is the `ast` object the daemon folds into a validate response
// when emit_ast is requested: SQL-shaped `_ast`/`node_defs`/`node_refs`/
// `_imports` rows from the SAME parse that produced the syntax verdict.
type ASTPayload struct {
	SourceID    string      `json:"source_id"`
	Language    string      `json:"language"`
	ContentHash string      `json:"content_hash"`
	AST         []ASTRow    `json:"ast"`
	Defs        []ASTDef    `json:"defs"`
	Refs        []ASTRef    `json:"refs"`
	Imports     []ASTImport `json:"imports"`
}

// ValidateResult is the decoded validate response.
type ValidateResult struct {
	OK     bool
	Errors []ValidateSyntaxError
	AST    *ASTPayload // non-nil only when emit_ast was requested and honored
}

// validateResponse is the raw wire shape. Errors is a *pointer* to a slice so
// key-absence (old daemon) is distinguishable from an empty array (clean
// parse) — the v0.7.8 contract always includes "errors", even when empty.
type validateResponse struct {
	OK     *bool                  `json:"ok"`
	Error  *string                `json:"error,omitempty"`
	Errors *[]ValidateSyntaxError `json:"errors"`
	AST    *ASTPayload            `json:"ast,omitempty"`
}

// Validate runs the daemon's validate op on caller-supplied content.
// language is a leyline extension key ("go", "py", ...); path is optional and
// (a) lets the daemon infer the language when language is empty, (b) becomes
// the source_id on emitted AST rows. emitAST folds ONE parse into both the
// syntax verdict and SQL-shaped AST rows (ValidateResult.AST).
//
// A daemon response without the `errors` key, or without the `ast` payload
// when emitAST was requested on a clean parse, is a hard error: the daemon
// predates the v0.7.8 wire contract and mache must not silently skip
// validation or linting.
func (c *SocketClient) Validate(content []byte, language, path string, emitAST bool) (*ValidateResult, error) {
	req := map[string]any{
		"op":      "validate",
		"content": string(content),
	}
	if language != "" {
		req["language"] = language
	}
	if path != "" {
		req["path"] = path
	}
	if emitAST {
		req["emit_ast"] = true
	}

	var resp validateResponse
	if err := c.SendOpInto(req, &resp); err != nil {
		return nil, fmt.Errorf("leyline validate op: %w", err)
	}
	if resp.Error != nil {
		// Structured daemon error envelope: unknown op (pre-v0.7.7 daemon),
		// unknown language, or missing language+path. All are hard errors —
		// the caller gates supported languages client-side, so none of these
		// is a legitimate pass-through.
		return nil, fmt.Errorf("leyline validate: %s (daemon must be ley-line-open >= %s)", *resp.Error, leylineBinaryVersion)
	}
	if resp.Errors == nil {
		return nil, fmt.Errorf("leyline validate response has no `errors` field — daemon too old, need ley-line-open >= %s (refusing to treat the write as validated)", leylineBinaryVersion)
	}

	res := &ValidateResult{
		OK:     resp.OK != nil && *resp.OK,
		Errors: *resp.Errors,
		AST:    resp.AST,
	}
	if emitAST && res.AST == nil {
		return nil, fmt.Errorf("leyline validate did not return the `ast` payload for emit_ast — daemon too old, need ley-line-open >= %s", leylineBinaryVersion)
	}
	return res, nil
}

// ValidateContent is the write-back path's convenience wrapper: it acquires a
// daemon via DiscoverOrStart (reusing a live socket when one exists — env
// LEYLINE_SOCKET or ~/.mache/default.sock — else lazily spawning a managed
// daemon), dials, runs one validate op, and closes the connection.
//
// Latency: the UDS dial + round trip is sub-millisecond when a daemon is
// already running. The FIRST write after mount may pay a cold daemon spawn
// (typically 0.5–2s: arena init + socket bind); subsequent writes reuse that
// daemon via the well-known socket. Dial-per-call is deliberate — it avoids a
// cached client going stale when the daemon restarts between writes.
func ValidateContent(content []byte, language, path string, emitAST bool) (*ValidateResult, error) {
	sock, err := DiscoverOrStart()
	if err != nil {
		return nil, fmt.Errorf("acquire leyline daemon for validate: %w", err)
	}
	c, err := DialSocket(sock)
	if err != nil {
		return nil, fmt.Errorf("dial leyline daemon %s: %w", sock, err)
	}
	defer func() { _ = c.Close() }()
	return c.Validate(content, language, path, emitAST)
}

// PinnedBinaryVersion returns the ley-line-open release tag this mache build
// pins (e.g. "v0.7.8"). Exported for test gates that must verify a local
// leyline binary matches the pin without ever downloading one.
func PinnedBinaryVersion() string { return leylineBinaryVersion }
