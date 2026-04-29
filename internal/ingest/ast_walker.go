package ingest

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/agentic-research/mache/internal/graph"
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

		// Apply #match? regex filters: capture text must match
		for _, mp := range pattern.matchPreds {
			val, ok := values[mp.capture].(string)
			if !ok || !mp.regex.MatchString(val) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}

		// Apply #not-match? regex filters: capture text must NOT match
		for _, mp := range pattern.notMatchPreds {
			val, ok := values[mp.capture].(string)
			if !ok || mp.regex.MatchString(val) {
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

// contextKindRegistry stores per-language top-level node kinds whose source
// text should be concatenated into the context blob (Go imports, consts,
// vars, type declarations, etc.).
var contextKindRegistry sync.Map // langName → []string (node_kinds)

// RegisterASTContextKinds registers the top-level node kinds whose source
// bytes constitute the context blob for the given language.
func RegisterASTContextKinds(langName string, kinds []string) {
	contextKindRegistry.Store(langName, kinds)
}

// ExtractContext returns the concatenated source text of the top-level
// context nodes for the given file (e.g. Go's import/const/var/type
// declarations). Used by schema context fields. Mirrors
// SitterWalker.ExtractContext but reads byte ranges from _ast and bytes
// from _source.
//
// Returns (nil, nil) when no context kinds are registered for the language.
func (w *ASTWalker) ExtractContext(sourcePath, langName string) ([]byte, error) {
	raw, ok := contextKindRegistry.Load(langName)
	if !ok {
		return nil, nil
	}
	kinds := raw.([]string)
	if len(kinds) == 0 {
		return nil, nil
	}

	sourceID := filepath.Base(sourcePath)
	source, err := w.readSource(w.db, sourceID)
	if err != nil || source == nil {
		return nil, err
	}

	// Build placeholders for IN (?, ?, ...).
	placeholders := strings.Repeat("?,", len(kinds))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(kinds)+1)
	for _, k := range kinds {
		args = append(args, k)
	}
	args = append(args, sourceID)

	// Top-level nodes have no `/` in their id (or one segment after the
	// source root, depending on schema). We pull all matching kinds for
	// this source_id ordered by start_byte and slice the source bytes.
	query := fmt.Sprintf(`
		SELECT a.start_byte, a.end_byte
		FROM _ast a
		WHERE a.node_kind IN (%s) AND a.source_id = ?
		ORDER BY a.start_byte ASC`, placeholders)
	rows, err := w.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("extract context: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var buf []byte
	seen := make(map[int]bool) // dedupe by start_byte
	for rows.Next() {
		var start, end int
		if err := rows.Scan(&start, &end); err != nil {
			continue
		}
		if seen[start] {
			continue
		}
		seen[start] = true
		if start < 0 || end > len(source) || start >= end {
			continue
		}
		buf = append(buf, source[start:end]...)
		buf = append(buf, '\n', '\n')
	}
	return buf, rows.Err()
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
	outerKind     string // the node kind to match (e.g., "function_declaration")
	captures      []selectorCapture
	predicates    []selectorPredicate      // #eq? filters
	matchPreds    []selectorMatchPredicate // #match? regex filters
	notMatchPreds []selectorMatchPredicate // #not-match? negated regex filters
}

type selectorCapture struct {
	kind     string   // leaf node kind to match (e.g., "type_identifier")
	name     string   // capture name (e.g., "receiver")
	ancestry []string // intermediate node kinds from outer to leaf (excluding scope and leaf itself), e.g. ["parameter_list", "parameter_declaration", "pointer_type"]
}

// selectorPredicate represents a #eq? filter: capture text must equal literal.
type selectorPredicate struct {
	capture string // capture name to check (e.g., "_type")
	literal string // expected text value (e.g., "resource")
}

// selectorMatchPredicate represents a #match? or #not-match? filter:
// capture text must (or must not) match the regex.
type selectorMatchPredicate struct {
	capture string         // capture name to check
	regex   *regexp.Regexp // compiled regex; tree-sitter regex syntax is RE2-compatible for the patterns we care about
	pattern string         // original pattern source (for error messages)
}

// parseSelector parses a tree-sitter S-expression into a selectorPattern.
// Builds ancestry chains for each capture so that nested type constraints
// (e.g., pointer_type > type_identifier vs bare type_identifier) are matched
// correctly against the _ast table's ID-path hierarchy.
//
// Supports #eq?, #match?, and #not-match? predicates. Captured text for
// match predicates is resolved from the _source byte ranges already populated
// by the capture loop. Other tree-sitter predicates (#not-eq?, #any-eq?,
// #is?, #is-not?) still require SitterWalker.
func parseSelector(selector string) (*selectorPattern, error) {
	s := strings.TrimSpace(selector)
	if s == "" {
		return nil, fmt.Errorf("empty selector")
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

	// Extract #eq? / #match? / #not-match? predicates
	for i := 0; i < len(tokens)-4; i++ {
		if tokens[i] != "(" {
			continue
		}
		predName := tokens[i+1]
		if predName != "#eq?" && predName != "#match?" && predName != "#not-match?" {
			continue
		}
		if i+4 >= len(tokens) || !strings.HasPrefix(tokens[i+2], "@") {
			continue
		}
		capName := tokens[i+2][1:]
		if capName == "" {
			continue
		}
		literal := strings.Trim(tokens[i+3], "\"")
		switch predName {
		case "#eq?":
			pattern.predicates = append(pattern.predicates, selectorPredicate{
				capture: capName,
				literal: literal,
			})
		case "#match?", "#not-match?":
			re, err := regexp.Compile(literal)
			if err != nil {
				return nil, fmt.Errorf("compile %s regex %q: %w", predName, literal, err)
			}
			mp := selectorMatchPredicate{capture: capName, regex: re, pattern: literal}
			if predName == "#match?" {
				pattern.matchPreds = append(pattern.matchPreds, mp)
			} else {
				pattern.notMatchPreds = append(pattern.notMatchPreds, mp)
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
// ancestorKinds accumulates the node-kind path from the root of the parse to
// this node's parent. The first entry is always the outerKind (scope).
// Captures record ancestry via ancestryFromKinds (skip scope, keep the rest).
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
					pattern.captures = append(pattern.captures, selectorCapture{
						kind:     nestedKind,
						name:     capName,
						ancestry: ancestryFromKinds(ancestorKinds),
					})
				}
			}
			continue
		}

		if strings.HasPrefix(tok, "@") {
			capName := tok[1:]
			pos++
			if capName != "scope" && capName != "" && capName[0] != '_' {
				pattern.captures = append(pattern.captures, selectorCapture{
					kind:     nodeKind,
					name:     capName,
					ancestry: ancestryFromKinds(ancestorKinds),
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
			pattern.captures = append(pattern.captures, selectorCapture{
				kind:     nodeKind,
				name:     capName,
				ancestry: ancestryFromKinds(ancestorKinds),
			})
		}
	}

	return pos
}

// ancestryFromKinds returns the intermediate node kinds between the scope and
// the leaf. ancestorKinds[0] is always the outerKind (scope) — skip it.
//
//	ancestorKinds=["call","arguments","call"] → ["arguments","call"]
func ancestryFromKinds(ancestorKinds []string) []string {
	if len(ancestorKinds) <= 1 {
		return nil
	}
	out := make([]string, len(ancestorKinds)-1)
	copy(out, ancestorKinds[1:])
	return out
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
//
// When ancestry is non-empty, the query constrains depth in SQL using a
// LIKE pattern with exactly len(ancestry)+1 path segments (ancestors + leaf),
// plus a NOT LIKE excluding deeper descendants. This avoids scanning all
// descendants in Go — only nodes at the exact expected depth are returned.
func (w *ASTWalker) findChildByKindAST(db *sql.DB, parentID, kind, sourceID string, ancestry []string) (*astNode, error) {
	const baseCols = `SELECT n.id, n.parent_id, n.name, n.kind, COALESCE(n.record, ''),
	        COALESCE(a.start_byte, 0), COALESCE(a.end_byte, 0)
	 FROM nodes n
	 JOIN _ast a ON a.node_id = n.id
	 WHERE `

	var query string
	var args []any

	if len(ancestry) == 0 {
		// No ancestry — direct child only (depth 1 below parent).
		// Tree-sitter S-expressions like `(parent (child) @cap)` mean
		// child is a direct child; we mirror that by constraining depth.
		query = baseCols + "n.id LIKE ? AND n.id NOT LIKE ? AND a.node_kind = ?"
		args = []any{parentID + "/%", parentID + "/%/%", kind}
	} else {
		// Ancestry constraint — restrict to exact depth.
		// For ancestry=["parameter_list","parameter_declaration"], build:
		//   LIKE 'parentID/%/%/%'         (3 segments: 2 ancestors + leaf)
		//   NOT LIKE 'parentID/%/%/%/%'   (exclude deeper nodes)
		depthPattern := parentID
		for range len(ancestry) + 1 {
			depthPattern += "/%"
		}
		query = baseCols + "n.id LIKE ? AND n.id NOT LIKE ? AND a.node_kind = ?"
		args = []any{depthPattern, depthPattern + "/%", kind}
	}

	if sourceID != "" {
		query += " AND a.source_id = ?"
		args = append(args, sourceID)
	}
	query += " ORDER BY a.start_byte ASC"

	if len(ancestry) == 0 {
		// No ancestry — first match wins.
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

	// With ancestry: depth is constrained in SQL, but we still verify
	// the exact kind sequence in Go (LIKE % wildcards don't check kinds).
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
