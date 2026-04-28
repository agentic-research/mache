package ingest

import (
	"database/sql"
	"fmt"
	"os"
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

// EnsureIndexes creates compound indexes on the _ast table for query
// performance. Call once after opening the DB, before concurrent queries.
// Transforms findNodesByKind from O(N) full table scan to O(K) index lookup.
// Returns an error if the index cannot be created (e.g., no _ast table,
// read-only DB, or connection pool exhausted).
func (w *ASTWalker) EnsureIndexes() error {
	_, err := w.db.Exec("CREATE INDEX IF NOT EXISTS idx_ast_kind_source ON _ast(node_kind, source_id)")
	return err
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

	// "$" is the wildcard selector — returns a single match representing
	// "everything at this level." Used by schema nodes like functions/,
	// types/, imports/ to create grouping containers. The Engine iterates
	// children of this match using nested schema nodes with real selectors.
	if selector == "$" {
		return []Match{&astMatch{
			values: map[string]any{},
			ctx:    ar,
		}}, nil
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

	// Identify required captures: any capture whose name doesn't start with "_"
	// is required. If it fails to resolve, the entire match is skipped.
	requiredCaptures := map[string]bool{}
	for _, cap := range pattern.captures {
		if cap.name != "scope" && !strings.HasPrefix(cap.name, "_") {
			requiredCaptures[cap.name] = true
		}
	}

	var matches []Match
	for _, scopeNode := range scopeNodes {
		values := make(map[string]any)
		captureRanges := make(map[string][2]int)
		missingRequired := false

		// Resolve captures from children (searches descendants, not just direct children)
		for _, cap := range pattern.captures {
			if cap.name == "scope" {
				continue
			}
			child, err := w.findChildByKindAST(ar.DB, scopeNode.id, cap.kind, ar.SourceID, cap.ancestry)
			if err != nil || child == nil {
				if requiredCaptures[cap.name] {
					missingRequired = true
					break
				}
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

		// Skip match if any required capture couldn't be resolved
		if missingRequired {
			continue
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

// ExtractGoImports reads Go import aliases from the _imports table
// (produced by ley-line-open's `leyline parse`). Returns alias → path map
// for qualified call resolution (e.g., auth.Validate → github.com/foo/auth).
// Mirrors SitterWalker.ExtractGoImports but uses SQL instead of CGO tree-sitter.
func (w *ASTWalker) ExtractGoImports(sourceID string) (map[string]string, error) {
	// Check if _imports table exists (older .dbs produced before LLO didn't have it)
	var count int
	if err := w.db.QueryRow(
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name='_imports'",
	).Scan(&count); err != nil || count == 0 {
		return nil, nil
	}

	rows, err := w.db.Query(
		"SELECT alias, path FROM _imports WHERE source_id = ?", sourceID,
	)
	if err != nil {
		return nil, fmt.Errorf("query _imports: %w", err)
	}
	defer func() { _ = rows.Close() }()

	imports := make(map[string]string)
	for rows.Next() {
		var alias, path string
		if err := rows.Scan(&alias, &path); err != nil {
			continue
		}
		imports[alias] = path
	}
	return imports, rows.Err()
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
	kind     string   // leaf node kind to match (e.g., "type_identifier")
	name     string   // capture name (e.g., "receiver")
	ancestry []string // required ancestor kinds from leaf to outer, e.g. ["pointer_type", "parameter_declaration", "parameter_list"]
}

// selectorPredicate represents a #eq? filter: capture text must equal literal.
type selectorPredicate struct {
	capture string // capture name to check (e.g., "_type")
	literal string // expected text value (e.g., "resource")
}

// parseSelector parses a tree-sitter S-expression into a selectorPattern.
// Builds ancestry chains for each capture so that nested type constraints
// (e.g., pointer_type > type_identifier vs bare type_identifier) are matched
// correctly against the _ast table's ID-path hierarchy.
//
// Supports #eq? predicates. Rejects #match? and other CGO-only predicates.
func parseSelector(selector string) (*selectorPattern, error) {
	s := strings.TrimSpace(selector)
	if s == "" {
		return nil, fmt.Errorf("empty selector")
	}
	if strings.Contains(s, "#match?") {
		return nil, fmt.Errorf("#match? predicates require SitterWalker (CGO)")
	}
	for _, un := range []string{"#not-eq?", "#any-eq?", "#is?", "#is-not?"} {
		if strings.Contains(s, un) {
			return nil, fmt.Errorf("%s predicates require SitterWalker (CGO)", un)
		}
	}

	// Tokenize the S-expression
	tokens := tokenizeSExpr(s)
	if len(tokens) == 0 {
		return nil, fmt.Errorf("empty selector after tokenize")
	}

	pattern := &selectorPattern{}
	pos := 0

	// Parse the outermost (kind ...) @scope
	_ = parseSExprNode(tokens, pos, nil, pattern)

	// Extract #eq? predicates
	for i := 0; i < len(tokens)-4; i++ {
		if tokens[i] == "(" && tokens[i+1] == "#eq?" {
			// (#eq? @capture "literal")
			if i+4 < len(tokens) && strings.HasPrefix(tokens[i+2], "@") {
				capName := tokens[i+2][1:]
				literal := strings.Trim(tokens[i+3], "\"")
				if capName != "" {
					pattern.predicates = append(pattern.predicates, selectorPredicate{
						capture: capName,
						literal: literal,
					})
				}
			}
		}
	}

	if pattern.outerKind == "" {
		return nil, fmt.Errorf("no node kind in selector: %s", selector)
	}

	return pattern, nil
}

// tokenizeSExpr splits an S-expression into tokens: "(", ")", identifiers, @captures, "field:", "#eq?", strings.
func tokenizeSExpr(s string) []string {
	var tokens []string
	i := 0
	for i < len(s) {
		ch := s[i]
		switch ch {
		case '(', ')':
			tokens = append(tokens, string(ch))
			i++
		case ' ', '\t', '\n':
			i++
		case '"':
			// Quoted string
			j := i + 1
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			if j < len(s) {
				j++ // consume closing quote
			}
			tokens = append(tokens, s[i:j])
			i = j
		default:
			// Identifier, @capture, field:, #predicate
			j := i
			for j < len(s) && s[j] != ' ' && s[j] != '(' && s[j] != ')' && s[j] != '\t' && s[j] != '\n' {
				j++
			}
			tokens = append(tokens, s[i:j])
			i = j
		}
	}
	return tokens
}

// parseSExprNode parses one (kind children...) node from tokens starting at pos.
// ancestorKinds is the stack of parent node kinds leading to this node.
// Returns the position after the closing paren.
func parseSExprNode(tokens []string, pos int, ancestorKinds []string, pattern *selectorPattern) int {
	if pos >= len(tokens) || tokens[pos] != "(" {
		return pos
	}
	pos++ // consume "("

	// Skip predicates like (#eq? ...)
	if pos < len(tokens) && strings.HasPrefix(tokens[pos], "#") {
		depth := 1
		for pos < len(tokens) && depth > 0 {
			switch tokens[pos] {
			case "(":
				depth++
			case ")":
				depth--
			}
			pos++
		}
		return pos
	}

	// First token after "(" is the node kind
	if pos >= len(tokens) {
		return pos
	}
	nodeKind := tokens[pos]
	pos++

	// Set outer kind if this is the first node
	if pattern.outerKind == "" {
		pattern.outerKind = nodeKind
	}

	// Process children: field: labels, nested (kind ...), @captures
	for pos < len(tokens) {
		tok := tokens[pos]

		if tok == ")" {
			pos++ // consume closing paren
			break
		}

		if strings.HasSuffix(tok, ":") {
			// field: label — skip it, next token is the child
			pos++
			continue
		}

		if tok == "(" {
			if pos+1 < len(tokens) && strings.HasPrefix(tokens[pos+1], "#") {
				// Predicate — skip
				depth := 1
				pos++
				for pos < len(tokens) && depth > 0 {
					switch tokens[pos] {
					case "(":
						depth++
					case ")":
						depth--
					}
					pos++
				}
				continue
			}
			// Remember the nested node's kind before recursing
			nestedKind := ""
			if pos+1 < len(tokens) && tokens[pos+1] != "(" && tokens[pos+1] != ")" && !strings.HasPrefix(tokens[pos+1], "#") {
				nestedKind = tokens[pos+1]
			}
			// Nested node — recurse with this node's kind added to ancestry
			pos = parseSExprNode(tokens, pos, append(ancestorKinds, nodeKind), pattern)
			// Check for @capture after the nested node's closing paren
			// e.g., (identifier) @_type — the capture belongs to "identifier", not "block"
			if pos < len(tokens) && strings.HasPrefix(tokens[pos], "@") && nestedKind != "" {
				capName := tokens[pos][1:]
				pos++
				if capName != "" && capName != "scope" {
					var ancestry []string
					for _, a := range ancestorKinds {
						if a != pattern.outerKind {
							ancestry = append(ancestry, a)
						}
					}
					pattern.captures = append(pattern.captures, selectorCapture{
						kind:     nestedKind,
						name:     capName,
						ancestry: ancestry,
					})
				}
			}
			continue
		}

		if strings.HasPrefix(tok, "@") {
			capName := tok[1:]
			pos++
			if capName != "scope" && capName != "" && capName[0] != '_' {
				// Build ancestry from outer to leaf (excluding outer kind which is pattern.outerKind)
				var ancestry []string
				for _, a := range ancestorKinds {
					if a != pattern.outerKind {
						ancestry = append(ancestry, a)
					}
				}
				pattern.captures = append(pattern.captures, selectorCapture{
					kind:     nodeKind,
					name:     capName,
					ancestry: ancestry,
				})
			} else if capName != "" && capName[0] == '_' {
				// Captures starting with _ are for #eq? predicates — still record them
				pattern.captures = append(pattern.captures, selectorCapture{
					kind: nodeKind,
					name: capName,
				})
			}
			continue
		}

		// Some other token — skip
		pos++
	}

	// Check for @capture after the closing paren (e.g., ") @scope", ") @_type")
	if pos < len(tokens) && strings.HasPrefix(tokens[pos], "@") {
		capName := tokens[pos][1:]
		pos++
		if capName != "scope" && capName != "" {
			var ancestry []string
			for _, a := range ancestorKinds {
				if a != pattern.outerKind {
					ancestry = append(ancestry, a)
				}
			}
			pattern.captures = append(pattern.captures, selectorCapture{
				kind:     nodeKind,
				name:     capName,
				ancestry: ancestry,
			})
		}
	}

	return pos
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

// findChildByKindAST finds the first descendant matching a node_kind via _ast table,
// optionally verifying that the node's ID path contains the required ancestor kinds.
// Ordered by start_byte ASC for deterministic first-occurrence behavior.
func (w *ASTWalker) findChildByKindAST(db *sql.DB, parentID, kind, sourceID string, ancestry []string) (*astNode, error) {
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
	query += " ORDER BY a.start_byte ASC"

	if len(ancestry) == 0 {
		// No ancestry constraint — return first match (old behavior)
		query += " LIMIT 1"
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

	// With ancestry constraint: check that the node's ID path between
	// parentID and the leaf contains the required ancestor kinds in order.
	// e.g., ancestry=["parameter_list","parameter_declaration","pointer_type"]
	// means the path should be: .../parameter_list.../parameter_declaration.../pointer_type.../type_identifier
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var n astNode
		if err := rows.Scan(&n.id, &n.parentID, &n.name, &n.kind, &n.record, &n.startByte, &n.endByte); err != nil {
			continue
		}
		// Check ancestry: the path segments between parentID and this node
		// should contain all ancestor kinds in order
		suffix := strings.TrimPrefix(n.id, parentID+"/")
		if matchAncestry(suffix, ancestry) {
			return &n, nil
		}
	}
	return nil, nil
}

// matchAncestry checks that the path from the scope node to the leaf node
// matches the expected ancestor chain EXACTLY. The ancestry slice lists the
// intermediate node kinds from outermost to innermost (excluding the scope
// and the leaf itself).
//
// For the pointer receiver selector:
//
//	ancestry=["parameter_list", "parameter_declaration", "pointer_type"]
//	matches: .../parameter_list_0/parameter_declaration/pointer_type/type_identifier ✓
//	rejects: .../parameter_list_0/parameter_declaration/type_identifier               ✗
//
// For the value receiver selector:
//
//	ancestry=["parameter_list", "parameter_declaration"]
//	matches: .../parameter_list_0/parameter_declaration/type_identifier               ✓
//	rejects: .../parameter_list_0/parameter_declaration/pointer_type/type_identifier   ✗
//
// "Exact" means every segment in the path between scope and leaf must be
// accounted for by the ancestry chain. Extra intermediate nodes cause rejection.
func matchAncestry(pathSuffix string, ancestry []string) bool {
	segments := strings.Split(pathSuffix, "/")
	// The last segment is the leaf node itself — exclude it
	if len(segments) > 0 {
		segments = segments[:len(segments)-1]
	}

	// Strip numeric suffixes from all segments
	stripped := make([]string, len(segments))
	for i, seg := range segments {
		stripped[i] = stripNumericSuffix(seg)
	}

	// The stripped segments must match the ancestry exactly
	if len(stripped) != len(ancestry) {
		return false
	}
	for i := range ancestry {
		if stripped[i] != ancestry[i] {
			return false
		}
	}
	return true
}

// stripNumericSuffix removes a trailing _N (e.g., "parameter_list_0" → "parameter_list").
func stripNumericSuffix(s string) string {
	idx := strings.LastIndexByte(s, '_')
	if idx <= 0 {
		return s
	}
	tail := s[idx+1:]
	for _, c := range tail {
		if c < '0' || c > '9' {
			return s
		}
	}
	return s[:idx]
}

// readSource reads the source content for a given source ID.
// Handles both inline BLOBs (content column) and file path references
// (path column, used when LLO stores references instead of content).
func (w *ASTWalker) readSource(db *sql.DB, sourceID string) ([]byte, error) {
	var content []byte
	var path sql.NullString
	err := db.QueryRow("SELECT content, path FROM _source WHERE id = ?", sourceID).Scan(&content, &path)
	if err != nil {
		return nil, err
	}
	// If content is inline, use it directly
	if len(content) > 0 {
		return content, nil
	}
	// Fall back to reading from disk via path reference
	if path.Valid && path.String != "" {
		return os.ReadFile(path.String)
	}
	return nil, fmt.Errorf("_source %s: no content and no path", sourceID)
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
