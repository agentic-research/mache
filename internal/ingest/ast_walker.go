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
// It does NOT mutate the connection: the walker may run against a shared,
// long-lived, SERVED database (serve/mount wire it onto a SQLiteGraph via
// pickCallExtractor), where changing the pool size or holding a file lock for
// the daemon's lifetime is harmful (mache-010123). Read-perf tuning that is
// only safe when mache exclusively OWNS the db (a one-shot build's temp _ast
// db) lives in TuneReadConnForBuild, which the build path opts into. The big
// projection speedup — the per-file in-memory node index (mache-4f3840) — is
// connection-count-agnostic and applies here regardless.
func NewASTWalker(db *sql.DB) *ASTWalker {
	return &ASTWalker{db: db}
}

// TuneReadConnForBuild applies aggressive read tuning that is ONLY safe when
// the caller exclusively owns db — i.e. a one-shot `mache build` over a private
// temp _ast db that ley-line already closed. It MUST NOT be called on a
// served/mounted or otherwise shared handle: SetMaxOpenConns(1) clobbers the
// SQLiteGraph's own pool and locking_mode=EXCLUSIVE holds a POSIX file lock for
// the connection's life, blocking every other reader/writer of that file
// (mache-010123).
//
// What it buys (mache-4f3840, whole-repo build): EXCLUSIVE eliminates the
// per-statement fcntl F_SETLK lock/unlock dance (~81% of CPU before the index
// fix cut query count); mmap turns page reads into memory-mapped accesses
// instead of pread syscalls; the large cache keeps the working set resident.
// Best-effort — a failed pragma degrades to slower-but-correct.
func TuneReadConnForBuild(db *sql.DB) {
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	_, _ = db.Exec("PRAGMA locking_mode = EXCLUSIVE")
	// Negative cache_size is in KiB: -262144 = 256 MiB.
	_, _ = db.Exec("PRAGMA cache_size = -262144")
	_, _ = db.Exec("PRAGMA temp_store = MEMORY")
	// 2 GiB mmap covers a whole-repo _ast db; a no-op where xFetch is unsupported.
	_, _ = db.Exec("PRAGMA mmap_size = 2147483648")
}

// InvalidateSource drops every per-file cache entry for sourceID so a
// subsequent query re-reads that file from the db. The mount/serve watcher
// calls this when a source file changes (mache-018eee) — without it,
// ReIngestFile would re-project from the walker's immortal caches and never see
// the edit.
//
// It also bounds cache growth on long-lived daemons: the per-file caches
// (indexCache/sourceCache/langCache/pkgCache/addrRefCache/callTokenCache)
// otherwise accumulate O(repo) node rows + source bytes for the walker's
// lifetime. That ceiling is bounded by the projected repo's file count; a
// one-shot `mache build` walker is short-lived so it needs no eviction, and a
// serve/mount daemon evicts per-file on change here (mache-024e9c).
func (w *ASTWalker) InvalidateSource(sourceID string) {
	w.indexCache.Delete(sourceID)
	w.sourceCache.Delete(sourceID)
	w.langCache.Delete(sourceID)
	w.pkgCache.Delete(sourceID)
	// addrRefCache/callTokenCache are keyed by sourceID+"\x00"+lang.
	prefix := sourceID + "\x00"
	for _, m := range []*sync.Map{&w.addrRefCache, &w.callTokenCache} {
		m.Range(func(k, _ any) bool {
			if ks, ok := k.(string); ok && strings.HasPrefix(ks, prefix) {
				m.Delete(ks)
			}
			return true
		})
	}
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

// ASTSourceID implements ASTScope — the real `_ast`/`_source` key for this
// match's file (NOT a graph node id; see bead mache-fd9982).
func (m *astMatch) ASTSourceID() string {
	return m.ctx.SourceID
}

// ASTScopeID implements ASTScope — the `_ast` scope node id this match's
// calls are constrained to (the same value ScopeCalls passes to
// ExtractCallsScoped as scopeID).
func (m *astMatch) ASTScopeID() string {
	return m.ctx.ParentPrefix
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
