package ingest

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// ASTWalker implements Walker by querying _ast and nodes tables produced
// by ley-line's ll-open/ts crate. This eliminates the CGO dependency on
// tree-sitter Go bindings — the AST was already parsed by Rust and stored
// in SQLite. Mache reads it via sqlite3_deserialize (zero-copy).
//
// See ADR-014 for the design rationale.
type ASTWalker struct {
	db *sql.DB
}

// NewASTWalker creates a walker backed by a SQLite database containing
// ley-line's _ast, _source, and nodes tables.
func NewASTWalker(db *sql.DB) *ASTWalker {
	return &ASTWalker{db: db}
}

// ASTRoot is the root context for ASTWalker queries. It scopes queries
// to a subtree of the AST via the parentPrefix.
type ASTRoot struct {
	DB           *sql.DB
	SourceID     string // which source file (key into _source)
	ParentPrefix string // scope queries to children under this prefix
}

// Query implements Walker. The selector is a tree-sitter S-expression pattern.
// ASTWalker translates it to SQL queries against the nodes and _ast tables.
//
// Currently supports the common pattern: (node_kind field: (child_kind) @capture) @scope,
// plus simple #eq? predicates over captured text. #match? requires SitterWalker.
func (w *ASTWalker) Query(root any, selector string) ([]Match, error) {
	ar, ok := root.(ASTRoot)
	if !ok {
		return nil, fmt.Errorf("ASTWalker.Query: expected ASTRoot, got %T", root)
	}

	pattern, err := parseSelector(selector)
	if err != nil {
		return nil, fmt.Errorf("parse selector: %w", err)
	}

	// Find all nodes matching the outer kind under the current scope
	scopeNodes, err := w.findNodesByKind(ar.DB, ar.ParentPrefix, pattern.outerKind, ar.SourceID)
	if err != nil {
		return nil, fmt.Errorf("find %s nodes: %w", pattern.outerKind, err)
	}

	// Read source content for byte-range extraction
	var source []byte
	if ar.SourceID != "" {
		source, _ = w.readSource(ar.DB, ar.SourceID)
	}

	var matches []Match
	for _, scopeNode := range scopeNodes {
		values := make(map[string]any)
		captureRanges := make(map[string][2]int)

		// Resolve captures from children (searches descendants, not just direct children)
		for _, cap := range pattern.captures {
			if cap.name == "scope" {
				continue // scope is the outer node itself
			}
			child, err := w.findChildByKindAST(ar.DB, scopeNode.id, cap.kind, ar.SourceID)
			if err != nil || child == nil {
				continue
			}
			// Record byte range for CaptureOrigin
			if child.startByte < child.endByte {
				captureRanges[cap.name] = [2]int{child.startByte, child.endByte}
			}
			// Leaf node: record column has the text
			if child.record != "" {
				values[cap.name] = child.record
			} else if source != nil && child.startByte < child.endByte {
				// Fall back to byte-range from source
				values[cap.name] = string(source[child.startByte:child.endByte])
			}
		}

		// Apply #eq? predicate filters
		skip := false
		for _, pred := range pattern.predicates {
			val, ok := values[pred.capture].(string)
			if !ok || val != pred.literal {
				skip = true
				break
			}
		}
		if skip {
			continue
		}

		// Build the match
		m := &astMatch{
			values:        values,
			captureRanges: captureRanges,
			ctx: ASTRoot{
				DB:           ar.DB,
				SourceID:     ar.SourceID,
				ParentPrefix: scopeNode.id,
			},
			startByte: scopeNode.startByte,
			endByte:   scopeNode.endByte,
		}
		matches = append(matches, m)
	}

	return matches, nil
}

// Close is a no-op — the ASTWalker doesn't own the database connection.
func (w *ASTWalker) Close() {}

// ExtractAddressRefs runs all registered address ref queries for the given
// language by querying the _ast table. Returns deduplicated, scheme-prefixed
// tokens (e.g., "env:DATABASE_URL"). Mirrors SitterWalker.ExtractAddressRefs
// but uses SQL instead of CGO tree-sitter.
func (w *ASTWalker) ExtractAddressRefs(sourcePath, langName string) ([]string, error) {
	raw, ok := addressRefRegistry.Load(langName)
	if !ok {
		return nil, nil
	}
	entries := raw.([]addressRefEntry)
	if len(entries) == 0 {
		return nil, nil
	}

	sourceID := filepath.Base(sourcePath)
	root := ASTRoot{DB: w.db, SourceID: sourceID, ParentPrefix: ""}

	seen := make(map[string]bool)
	var tokens []string

	for _, entry := range entries {
		matches, err := w.Query(root, entry.Query)
		if err != nil {
			continue // selector may not be supported by ASTWalker
		}
		for _, m := range matches {
			vals := m.Values()
			refVal, ok := vals["ref"].(string)
			if !ok || refVal == "" {
				continue
			}
			value := unquoteCapture(refVal)
			if value == "" {
				continue
			}
			token := entry.Scheme + ":" + value
			if !seen[token] {
				seen[token] = true
				tokens = append(tokens, token)
			}
		}
	}

	return tokens, nil
}

// SelectWalker inspects a SQLite database and returns the best Walker.
// If the database has an _ast table (produced by ley-line's ll-open/ts),
// returns an ASTWalker (pure Go, no CGO). Otherwise returns a SitterWalker
// (requires CGO tree-sitter bindings).
func SelectWalker(db *sql.DB) (Walker, error) {
	var count int
	err := db.QueryRow(
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name='_ast'",
	).Scan(&count)
	if err != nil {
		return nil, fmt.Errorf("inspect sqlite_master for _ast table: %w", err)
	}
	if count > 0 {
		return NewASTWalker(db), nil
	}
	return NewSitterWalker(), nil
}

// --- Internal types ---

type astNode struct {
	id        string
	parentID  string
	name      string
	kind      int // 0=file, 1=dir
	record    string
	startByte int
	endByte   int
}

type selectorPattern struct {
	outerKind  string // the node kind to match (e.g., "function_declaration")
	captures   []selectorCapture
	predicates []selectorPredicate // #eq? filters
}

type selectorCapture struct {
	kind string // child node kind (e.g., "identifier")
	name string // capture name (e.g., "name")
}

// selectorPredicate represents a #eq? filter: capture text must equal literal.
type selectorPredicate struct {
	capture string // capture name to check (e.g., "_type")
	literal string // expected text value (e.g., "resource")
}

// parseSelector parses a tree-sitter S-expression into a simple pattern.
// Handles the common forms used in mache schemas:
//
//	(function_declaration name: (identifier) @name) @scope
//	(type_declaration (type_spec name: (type_identifier) @name) @scope)
//
// Supports simple #eq? predicates by extracting them into pattern.predicates.
// Returns an error for patterns with #match?, #not-eq?, #any-eq?, #is?, #is-not?
// or other complex syntax that requires the full tree-sitter query engine.
func parseSelector(selector string) (*selectorPattern, error) {
	s := strings.TrimSpace(selector)
	if s == "" {
		return nil, fmt.Errorf("empty selector")
	}
	// Reject predicates we can't handle. #eq? is supported (extracted below).
	// #match? and all others require the full tree-sitter query engine.
	if strings.Contains(s, "#match?") {
		return nil, fmt.Errorf("#match? predicates require SitterWalker (CGO)")
	}
	for _, unsupported := range []string{"#not-eq?", "#any-eq?", "#is?", "#is-not?"} {
		if strings.Contains(s, unsupported) {
			return nil, fmt.Errorf("%s predicates require SitterWalker (CGO)", unsupported)
		}
	}

	pattern := &selectorPattern{}

	// Scan for all @capture tokens and the (kind) immediately before each.
	// Also find the outermost node kind (first identifier after the first open paren).
	//
	// Strategy: find every @name token, then look backward for the preceding (kind).
	// The outer kind is the first word after the first '('.

	// Find outer kind: first word after first '('
	if idx := strings.IndexByte(s, '('); idx >= 0 {
		rest := strings.TrimSpace(s[idx+1:])
		if spIdx := strings.IndexAny(rest, " ()"); spIdx > 0 {
			pattern.outerKind = rest[:spIdx]
		}
	}

	// Find all @captures with their preceding kinds.
	// Walk the string looking for @name tokens (not @scope).
	for i := 0; i < len(s); i++ {
		if s[i] != '@' {
			continue
		}
		// Extract capture name
		nameStart := i + 1
		nameEnd := nameStart
		for nameEnd < len(s) && s[nameEnd] != ' ' && s[nameEnd] != ')' {
			nameEnd++
		}
		captureName := s[nameStart:nameEnd]
		if captureName == "scope" || captureName == "" {
			i = nameEnd
			continue
		}

		// Look backward for the preceding (kind) — the last ')' before this '@'
		// then find its matching '(' to extract the kind name.
		j := i - 1
		for j >= 0 && s[j] == ' ' {
			j--
		}
		if j >= 0 && s[j] == ')' {
			// Find matching open paren
			depth := 0
			k := j
			for k >= 0 {
				if s[k] == ')' {
					depth++
				} else if s[k] == '(' {
					depth--
					if depth == 0 {
						break
					}
				}
				k--
			}
			if k >= 0 {
				inner := s[k+1 : j]
				// The kind is the last bare identifier in the inner text
				// e.g., from "type_spec name: (type_identifier)" → "type_identifier"
				// e.g., from "identifier" → "identifier"
				kind := extractLastKind(inner)
				if kind != "" {
					pattern.captures = append(pattern.captures, selectorCapture{
						kind: kind,
						name: captureName,
					})
				}
			}
		}
		i = nameEnd
	}

	// Extract #eq? predicates: (#eq? @capture "literal")
	// Scan-based approach: find each occurrence without mutating the string.
	{
		const marker = "(#eq?"
		offset := 0
		for {
			idx := strings.Index(s[offset:], marker)
			if idx < 0 {
				break
			}
			absIdx := offset + idx
			rest := s[absIdx+len(marker):]
			closeIdx := strings.IndexByte(rest, ')')
			if closeIdx < 0 {
				break
			}
			body := strings.TrimSpace(rest[:closeIdx])
			parts := strings.Fields(body)
			if len(parts) >= 2 && strings.HasPrefix(parts[0], "@") {
				capName := parts[0][1:]
				if capName != "" {
					literal := strings.Trim(parts[1], "\"\\")
					pattern.predicates = append(pattern.predicates, selectorPredicate{
						capture: capName,
						literal: literal,
					})
				}
			}
			offset = absIdx + len(marker) + closeIdx + 1
		}
	}

	if pattern.outerKind == "" {
		return nil, fmt.Errorf("no node kind in selector: %s", selector)
	}

	return pattern, nil
}

// extractLastKind finds the last bare node kind in an S-expression fragment.
// "type_spec name: (type_identifier)" → "type_identifier"
// "identifier" → "identifier"
func extractLastKind(s string) string {
	s = strings.TrimSpace(s)
	// If there's a nested group, extract the kind from the last one
	lastOpen := strings.LastIndexByte(s, '(')
	if lastOpen >= 0 {
		rest := s[lastOpen+1:]
		if spIdx := strings.IndexAny(rest, " )"); spIdx > 0 {
			return rest[:spIdx]
		}
		return strings.TrimRight(rest, ")")
	}
	// No parens — the whole thing is the kind (possibly with field: prefix)
	parts := strings.Fields(s)
	for i := len(parts) - 1; i >= 0; i-- {
		p := parts[i]
		if !strings.HasSuffix(p, ":") && !strings.HasPrefix(p, "@") {
			return strings.Trim(p, "()")
		}
	}
	return ""
}

// findNodesByKind finds all nodes of a specific kind under a parent prefix.
// Ley-line disambiguates siblings of the same kind with suffixes (e.g.,
// function_declaration_0, function_declaration_1). We match by _ast.node_kind
// which stores the original tree-sitter kind without suffixes.
func (w *ASTWalker) findNodesByKind(db *sql.DB, parentPrefix, kind, sourceID string) ([]astNode, error) {
	query := `SELECT n.id, n.parent_id, n.name, n.kind, COALESCE(n.record, ''),
	                COALESCE(a.start_byte, 0), COALESCE(a.end_byte, 0)
	         FROM nodes n
	         JOIN _ast a ON a.node_id = n.id
	         WHERE a.node_kind = ?`
	args := []any{kind}

	if parentPrefix != "" {
		query += " AND n.id LIKE ?"
		args = append(args, parentPrefix+"/%")
	}
	if sourceID != "" {
		query += " AND a.source_id = ?"
		args = append(args, sourceID)
	}

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var nodes []astNode
	for rows.Next() {
		var n astNode
		if err := rows.Scan(&n.id, &n.parentID, &n.name, &n.kind, &n.record,
			&n.startByte, &n.endByte); err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

// findChildByKindAST finds the first descendant matching a node_kind via _ast table.
// Ordered by start_byte ASC for deterministic first-occurrence behavior (matches
// tree-sitter's document-order traversal). Scoped to sourceID when non-empty.
func (w *ASTWalker) findChildByKindAST(db *sql.DB, parentID, kind, sourceID string) (*astNode, error) {
	query := `SELECT n.id, n.parent_id, n.name, n.kind, COALESCE(n.record, ''),
	        COALESCE(a.start_byte, 0), COALESCE(a.end_byte, 0)
	 FROM nodes n
	 JOIN _ast a ON a.node_id = n.id
	 WHERE n.id LIKE ? AND a.node_kind = ?`
	args := []any{parentID + "/%", kind}
	if sourceID != "" {
		query += " AND a.source_id = ?"
		args = append(args, sourceID)
	}
	query += " ORDER BY a.start_byte ASC LIMIT 1"

	var n astNode
	err := db.QueryRow(query, args...).Scan(&n.id, &n.parentID, &n.name, &n.kind, &n.record, &n.startByte, &n.endByte)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// readSource reads the source content for a given source ID.
func (w *ASTWalker) readSource(db *sql.DB, sourceID string) ([]byte, error) {
	var content []byte
	err := db.QueryRow("SELECT content FROM _source WHERE id = ?", sourceID).Scan(&content)
	if err != nil {
		return nil, err
	}
	return content, nil
}

// --- Match implementation ---

type astMatch struct {
	values        map[string]any
	captureRanges map[string][2]int // capture name → [startByte, endByte]
	ctx           ASTRoot
	startByte     int
	endByte       int
}

func (m *astMatch) Values() map[string]any { return m.values }
func (m *astMatch) Context() any           { return m.ctx }

// CaptureOrigin satisfies OriginProvider for write-back support.
// Returns byte ranges for @scope (the outer matched node) and any named
// captures whose byte ranges were recorded during Query.
func (m *astMatch) CaptureOrigin(name string) (uint32, uint32, bool) {
	if name == "scope" {
		return uint32(m.startByte), uint32(m.endByte), true
	}
	if r, ok := m.captureRanges[name]; ok && r[0] < r[1] {
		return uint32(r[0]), uint32(r[1]), true
	}
	return 0, 0, false
}
