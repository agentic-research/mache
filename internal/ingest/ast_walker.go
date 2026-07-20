package ingest

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"

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
	// langCache/pkgCache memoize per-file FileMeta keyed by source_id. The
	// engine calls Lang()/PackageName() for every construct node, but both are
	// file-level facts — caching avoids an N+1 SQL pattern on the ingest path.
	langCache   sync.Map // sourceID -> string
	pkgCache    sync.Map // sourceID -> string
	sourceCache sync.Map // sourceID -> []byte
	// addrRefCache memoizes the whole-file address-ref extraction keyed by
	// sourceID+"\x00"+lang. Per-construct ExtractAddressRefsScoped filters these
	// by node-id prefix instead of re-running the generic Query per construct —
	// that per-construct storm was 84% of a whole-repo projection (mache-4f3840).
	addrRefCache sync.Map // sourceID+"\x00"+lang -> []scopedAddrRef
	// indexCache holds the per-file in-memory node index. Materializing every
	// file's nodes/_ast rows ONCE and answering navigation from memory restores
	// the O(nodes) tree walk that per-node SQL had turned into O(nodes²)
	// (mache-4f3840). Keyed by sourceID.
	indexCache sync.Map // sourceID -> *fileIndex
	// callTokenCache memoizes the whole-file call extraction (leaf id + token)
	// per (sourceID, lang) so ExtractCallsScoped attributes by node-id prefix
	// instead of re-running queryCallPattern per construct.
	callTokenCache sync.Map // sourceID+"\x00"+lang -> []scopedCallToken
}

// scopedAddrRef is one whole-file address-ref match: the token plus the id of
// the AST node it was captured on, so per-construct callers can attribute it by
// node-id prefix.
type scopedAddrRef struct {
	nodeID string
	token  string
}

// fileLang returns the source language for sourceID, computed once and cached.
func (w *ASTWalker) fileLang(sourceID string) string {
	if v, ok := w.langCache.Load(sourceID); ok {
		return v.(string)
	}
	var lang string
	if sourceID != "" {
		_ = w.db.QueryRow("SELECT language FROM _source WHERE id = ?", sourceID).Scan(&lang)
	}
	w.langCache.Store(sourceID, lang)
	return lang
}

// filePkg returns the package name for sourceID (Go only), computed once and
// cached. Non-Go files resolve to "" with no per-node SQL.
func (w *ASTWalker) filePkg(sourceID string) string {
	if v, ok := w.pkgCache.Load(sourceID); ok {
		return v.(string)
	}
	pkg := ""
	if w.fileLang(sourceID) == "go" && sourceID != "" {
		// package_identifier under the package_clause — the SQL mirror of
		// SitterWalker's extractGoPackageName. Text from nodes.record, else the
		// _source byte range.
		var rec string
		var sb, eb int
		err := w.db.QueryRow(`SELECT COALESCE(n.record, ''), a.start_byte, a.end_byte
			FROM _ast a JOIN nodes n ON n.id = a.node_id
			WHERE a.source_id = ? AND a.node_kind = 'package_identifier'
			ORDER BY a.start_byte LIMIT 1`, sourceID).Scan(&rec, &sb, &eb)
		switch {
		case err != nil:
			pkg = ""
		case rec != "":
			pkg = rec
		default:
			var content []byte
			if e := w.db.QueryRow("SELECT content FROM _source WHERE id = ?", sourceID).Scan(&content); e == nil {
				if sb >= 0 && sb < eb && eb <= len(content) {
					pkg = string(content[sb:eb])
				}
			}
		}
	}
	w.pkgCache.Store(sourceID, pkg)
	return pkg
}

// fileSource returns the source bytes for sourceID, cached per file.
func (w *ASTWalker) fileSource(sourceID string) []byte {
	if v, ok := w.sourceCache.Load(sourceID); ok {
		return v.([]byte)
	}
	src, _ := w.readSource(w.db, sourceID)
	w.sourceCache.Store(sourceID, src)
	return src
}

// docExtendStart walks backward from a scope node over contiguous preceding
// comment siblings (same parent, <= 2 byte gap), returning the doc-extended
// start byte. The SQL mirror of SitterWalker's PrevSibling comment scan.
func (w *ASTWalker) docExtendStart(sourceID, scopeID string, scopeStart uint32) uint32 {
	var parentID string
	if err := w.db.QueryRow("SELECT parent_id FROM nodes WHERE id = ?", scopeID).Scan(&parentID); err != nil {
		return scopeStart
	}
	start := scopeStart
	for {
		// Closest preceding comment sibling (same parent), by end_byte.
		var cs, ce int
		err := w.db.QueryRow(`SELECT a.start_byte, a.end_byte
			FROM _ast a JOIN nodes n ON n.id = a.node_id
			WHERE n.parent_id = ? AND a.source_id = ? AND a.node_kind = 'comment' AND a.end_byte <= ?
			ORDER BY a.end_byte DESC LIMIT 1`, parentID, sourceID, int(start)).Scan(&cs, &ce)
		if err != nil || int(start)-ce > 2 {
			break
		}
		start = uint32(cs)
	}
	return start
}

// NewASTWalker creates a walker backed by a SQLite database containing
// ley-line's _ast, _source, and nodes tables.
//
// It tunes the read connection for the projection's access pattern: a very
// high volume of point/range B-tree seeks (findNodesByKind + per-capture
// findChildByKindAST for every construct). Two SQLite/database-sql facts make
// the untuned default pathological on a whole-repo projection (mache-4f3840,
// where 94% of CPU sat in syscall.rawsyscalln under sqlite3BtreeMoveto —
// pages re-read from disk via pread on nearly every seek):
//
//  1. The SQLite page cache is PER-CONNECTION, and database/sql's default pool
//     rotates queries across connections — each with a COLD cache — so the
//     working set never stays resident.
//  2. The default cache (~2 MB) is far smaller than a parsed repo's _ast/nodes
//     b-trees, so even one connection thrashes under LRU eviction.
//
// Pinning to a single persistent connection with a large cache keeps the
// working set warm across every seek. Reads serialize on the one connection,
// but the projection already consumes each result set fully before issuing
// nested queries (no single goroutine ever needs two connections at once), so
// this cannot deadlock — and the cache-warmth win dwarfs any lost read
// parallelism. tuneReadConn is best-effort: a failed pragma degrades to the
// old slow-but-correct behavior rather than failing the build.
func NewASTWalker(db *sql.DB) *ASTWalker {
	tuneReadConn(db)
	return &ASTWalker{db: db}
}

// tuneReadConn pins db to one persistent connection and grows its page cache,
// so PRAGMA cache_size (per-connection) sticks and the projection's B-tree
// seeks hit a warm cache instead of re-reading pages from disk. See
// NewASTWalker for why this is load-bearing (mache-4f3840).
func tuneReadConn(db *sql.DB) {
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	// locking_mode=EXCLUSIVE is the load-bearing pragma: without it, every
	// db.Query is its own implicit transaction that acquires AND releases a
	// POSIX file lock (fcntl F_SETLK) on the db file. Across the projection's
	// hundreds of thousands of tiny read queries that lock/unlock dance was
	// ~81% of CPU (mache-4f3840). EXCLUSIVE acquires the lock ONCE and holds it
	// for the connection's life, so subsequent statements skip the fcntl
	// syscalls entirely. Safe because the temp _ast db is exclusively ours
	// (ley-line already closed it) and we pinned a single connection above.
	_, _ = db.Exec("PRAGMA locking_mode = EXCLUSIVE")
	// Negative cache_size is in KiB: -262144 = 256 MiB, comfortably larger
	// than the b-trees for a whole-repo projection so the working set stays
	// resident once the lock overhead is gone.
	_, _ = db.Exec("PRAGMA cache_size = -262144")
	_, _ = db.Exec("PRAGMA temp_store = MEMORY")
	// mmap the db so page reads are memory-mapped accesses rather than pread
	// syscalls. 2 GiB covers a whole-repo _ast db; a no-op on drivers/platforms
	// without xFetch mmap support, so it can only help.
	_, _ = db.Exec("PRAGMA mmap_size = 2147483648")
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
			w:      w,
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

	// Read source content for byte-range extraction — via the per-file
	// sourceCache (fileSource), NOT a raw readSource. Query runs once per
	// schema selector per file, so an uncached read re-fetched+decompressed
	// the full file content ~N-selectors times per file; on a whole-repo
	// projection that was the dominant cost (mache-4f3840).
	var source []byte
	if ar.SourceID != "" {
		source = w.fileSource(ar.SourceID)
	}

	// Identify required captures: any capture whose name doesn't start with "_"
	// is required. If it fails to resolve, the entire match is skipped.
	requiredCaptures := map[string]bool{}
	for _, cap := range pattern.captures {
		if cap.name != "scope" && !strings.HasPrefix(cap.name, "_") {
			requiredCaptures[cap.name] = true
		}
	}

	// Expand each outer node into scope units. When @scope is the outer node
	// (the common case), there is one unit per outer node. When @scope is an
	// INNER node kind (e.g. `(type_declaration (type_spec ...) @scope)`), a
	// single outer node expands to one unit PER inner scope node — so grouped
	// declarations like `type ( Alpha; Beta )` project each member, matching
	// tree-sitter's one-match-per-inner-node semantics.
	innerScope := pattern.scopeKind != "" && pattern.scopeKind != pattern.outerKind
	var scopePrefix []string
	if innerScope {
		scopePrefix = append(append([]string{}, pattern.scopeAncestry...), pattern.scopeKind)
	}

	type scopeUnit struct {
		outerID string  // capture base for captures NOT under the @scope
		scope   astNode // the @scope node: text/range/ParentPrefix + capture base for captures under it
	}
	var units []scopeUnit
	for _, scopeNode := range scopeNodes {
		if !innerScope {
			units = append(units, scopeUnit{outerID: scopeNode.id, scope: scopeNode})
			continue
		}
		inners, err := w.findChildrenByKindAST(ar.DB, scopeNode.id, pattern.scopeKind, ar.SourceID, pattern.scopeAncestry)
		if err != nil {
			return nil, fmt.Errorf("find inner scope %s: %w", pattern.scopeKind, err)
		}
		if len(inners) == 0 {
			// No inner scope resolved — fall back to the outer node as the scope
			// (preserves the prior single-node behavior for odd shapes).
			units = append(units, scopeUnit{outerID: scopeNode.id, scope: scopeNode})
			continue
		}
		for _, in := range inners {
			units = append(units, scopeUnit{outerID: scopeNode.id, scope: in})
		}
	}

	var matches []Match
	for _, unit := range units {
		values := make(map[string]any)
		captureRanges := make(map[string][2]int)
		missingRequired := false

		scopeForText := unit.scope
		// Inject the scope node's source text as "scope", mirroring SitterWalker —
		// so leaf templates like {{.scope}} (the most common source-leaf template)
		// render the construct's source instead of "<no value>".
		if source != nil && scopeForText.startByte < scopeForText.endByte && scopeForText.endByte <= len(source) {
			values["scope"] = string(source[scopeForText.startByte:scopeForText.endByte])
		}

		// Resolve captures from children (searches descendants, not just direct children).
		// parseSelector strips @scope captures before they reach pattern.captures,
		// so the explicit "scope" guard below is defensive — it can't be exercised
		// from the public API today, but is kept in case the selector grammar evolves.
		for _, cap := range pattern.captures {
			if cap.name == "scope" { // coverage:ignore — see comment above; unreachable via public API
				continue // coverage:ignore — same as above
			}
			// Captures nested under the @scope node resolve relative to the inner
			// scope (with the scope prefix stripped from their ancestry); captures
			// elsewhere resolve relative to the outer node with full ancestry.
			base := unit.outerID
			ancestry := cap.ancestry
			if innerScope && ancestryHasPrefix(cap.ancestry, scopePrefix) {
				base = unit.scope.id
				ancestry = cap.ancestry[len(scopePrefix):]
			}
			child, err := w.findChildByKindAST(ar.DB, base, cap.kind, ar.SourceID, ancestry)
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
				DB:       ar.DB,
				SourceID: ar.SourceID,
				// Scope nested schema-child queries to the resolved @scope node
				// (which may be an inner node like type_spec), mirroring
				// SitterWalker.Context() returning the captured scope node.
				ParentPrefix: scopeForText.id,
			},
			startByte: scopeForText.startByte,
			endByte:   scopeForText.endByte,
			w:         w,
		}
		matches = append(matches, m)
	}

	return matches, nil
}

// Close is a no-op — the ASTWalker doesn't own the database connection.
func (w *ASTWalker) Close() {}

// SelectWalker inspects a SQLite database and returns the ASTWalker when the
// database has an `_ast` table (produced by ley-line's ll-open/ts). Since
// ADR-0012 step 4 removed in-process CGO tree-sitter, a database WITHOUT an
// `_ast` table is an error — there is no fallback walker. Callers must parse
// source through ley-line first (see runBuildViaLeylineSchema / autoInvokeLeylineParse).
func SelectWalker(db *sql.DB) (Walker, error) {
	var count int
	err := db.QueryRow(
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name='_ast'",
	).Scan(&count)
	if err != nil {
		return nil, fmt.Errorf("inspect sqlite_master for _ast table: %w", err)
	}
	if count == 0 {
		return nil, fmt.Errorf("no _ast table in database: mache requires a " +
			"ley-line-parsed source db (in-process tree-sitter was removed in ADR-0012 step 4)")
	}
	return NewASTWalker(db), nil
}

// --- Internal types ---

// astMatch is the Match returned by ASTWalker.Query. Values holds resolved
// capture text, captureRanges records byte ranges for write-back, and ctx
// carries the ASTRoot scoped to the matched node.
type astMatch struct {
	values        map[string]any
	captureRanges map[string][2]int // capture name → [startByte, endByte]
	ctx           ASTRoot
	startByte     int
	endByte       int
	w             *ASTWalker // for cached FileMeta (lang/pkg) lookups
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

// Lang implements FileMeta — the file's source language (cached on the walker,
// so the engine's per-construct calls don't issue per-node SQL).
func (m *astMatch) Lang() string {
	if m.w == nil {
		return ""
	}
	return m.w.fileLang(m.ctx.SourceID)
}

// PackageName implements FileMeta — the file's Go package (cached on the
// walker; "" for non-Go). SQL mirror of SitterWalker's extractGoPackageName.
func (m *astMatch) PackageName() string {
	if m.w == nil {
		return ""
	}
	return m.w.filePkg(m.ctx.SourceID)
}

// ScopeSource implements DocScope — the file's source bytes (cached).
func (m *astMatch) ScopeSource() []byte {
	if m.w == nil {
		return nil
	}
	return m.w.fileSource(m.ctx.SourceID)
}

// DocRange implements DocScope. The scope node's id is ctx.ParentPrefix (set in
// Query); a zero-width range marks a "$" grouping match with no real scope.
func (m *astMatch) DocRange() (docStart, scopeStart, scopeEnd uint32, ok bool) {
	if m.w == nil || m.endByte <= m.startByte {
		return 0, 0, 0, false
	}
	scopeStart = uint32(m.startByte)
	scopeEnd = uint32(m.endByte)
	docStart = m.w.docExtendStart(m.ctx.SourceID, m.ctx.ParentPrefix, scopeStart)
	return docStart, scopeStart, scopeEnd, true
}

// ScopeCalls implements CallExtractor — scope-prefixed calls + address-refs over
// _ast (the per-construct equivalent of SitterWalker's scope-node query).
func (m *astMatch) ScopeCalls() []string {
	if m.w == nil || m.endByte <= m.startByte {
		return nil
	}
	lang := m.w.fileLang(m.ctx.SourceID)
	calls, _ := m.w.ExtractCallsScoped(m.ctx.SourceID, m.ctx.ParentPrefix, lang)
	if addr, err := m.w.ExtractAddressRefsScoped(m.ctx.SourceID, m.ctx.ParentPrefix, lang); err == nil {
		calls = append(calls, addr...)
	}
	return calls
}

type astNode struct {
	id        string
	parentID  string
	name      string
	kind      int // 0=file, 1=dir
	record    string
	startByte int
	endByte   int
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
