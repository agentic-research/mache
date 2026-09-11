package ingest

import (
	"database/sql"
	"fmt"
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
	// addrRefCache memoizes the whole-file address-ref extraction keyed by
	// sourceID+"\x00"+lang. Per-construct ExtractAddressRefsScoped filters these
	// by node-id prefix instead of re-running the generic Query per construct —
	// that per-construct storm was 84% of a whole-repo projection (mache-4f3840).
	addrRefCache sync.Map // sourceID+"\x00"+lang -> []scopedAddrRef
	// indexCache holds each file's section of the db (fileIndex): its nodes,
	// lazily its source. Materializing a file's nodes/_ast rows ONCE and
	// answering navigation from memory restores the O(nodes) tree walk that
	// per-node SQL had turned into O(nodes²) (mache-4f3840); the language,
	// package, doc comments, calls, context and imports read the same rows,
	// so they come from here too (mache-40ce82). Keyed by sourceID.
	indexCache sync.Map // sourceID -> *fileIndex
	// callTokenCache memoizes the whole-file call extraction (leaf id + token)
	// per (sourceID, lang) so ExtractCallsScoped attributes by node-id prefix
	// instead of re-evaluating every call pattern per construct.
	callTokenCache sync.Map // sourceID+"\x00"+lang -> []scopedCallToken
	// importsProbed/hasImports remember whether the db has an _imports table
	// (see hasImportsTable) — a per-db fact, asked once, not once per file.
	importsMu     sync.Mutex
	importsProbed bool
	hasImports    bool
}

// scopedAddrRef is one whole-file address-ref match: the token plus the id of
// the AST node it was captured on, so per-construct callers can attribute it by
// node-id prefix.
type scopedAddrRef struct {
	nodeID string
	token  string
}

// fileLang returns the source language for sourceID — a file-level fact the
// engine asks for on every construct, answered from the file's section.
func (w *ASTWalker) fileLang(sourceID string) string {
	if idx := w.sectionOrNil(sourceID); idx != nil {
		return idx.lang
	}
	return ""
}

// filePkg returns the package name for sourceID (Go only; "" otherwise).
func (w *ASTWalker) filePkg(sourceID string) string {
	if idx := w.sectionOrNil(sourceID); idx != nil && idx.lang == "go" {
		return idx.packageName()
	}
	return ""
}

// fileSource returns the source bytes for sourceID, nil when unavailable.
func (w *ASTWalker) fileSource(sourceID string) []byte {
	if idx := w.sectionOrNil(sourceID); idx != nil {
		return idx.source
	}
	return nil
}

// sectionOrNil is fileSection for the FileMeta/DocScope accessors, whose
// interfaces have no error to return: an empty sourceID (the whole-db serve
// path) or a failed index load yields nil, and the accessors answer "" / nil
// as they always have for a file the db does not hold.
func (w *ASTWalker) sectionOrNil(sourceID string) *fileIndex {
	if sourceID == "" {
		return nil
	}
	idx, err := w.fileSection(sourceID)
	if err != nil {
		return nil
	}
	return idx
}

// docExtendStart is fileIndex.docExtendStart for the match's file; a file the
// db does not hold extends nothing.
func (w *ASTWalker) docExtendStart(sourceID, scopeID string, scopeStart uint32) uint32 {
	idx, err := w.fileIndex(sourceID)
	if err != nil {
		return scopeStart
	}
	return idx.docExtendStart(scopeID, scopeStart)
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
// It also bounds cache growth: the per-file caches
// (indexCache/addrRefCache/callTokenCache)
// otherwise accumulate O(repo) node rows + source bytes for the walker's
// lifetime. A serve/mount daemon evicts per-file on change here
// (mache-024e9c), and the engine evicts each file as soon as its projection
// is written (processSourceFileResult) — a one-shot build walker is
// short-lived, but for that lifetime it held every file's full index at once,
// which was the build's peak RSS, not a rounding error (mache-95a33d).
func (w *ASTWalker) InvalidateSource(sourceID string) {
	w.indexCache.Delete(sourceID)
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

// Query implements Walker. The selector is a tree-sitter S-expression pattern,
// evaluated against the file's in-memory section of the nodes⋈_ast tables.
//
// Supports the common pattern: (node_kind field: (child_kind) @capture) @scope,
// plus simple #eq? / #match? predicates over captured text. Field labels
// constrain like tree-sitter's: a labelled step reaches only the child under
// that field, an unlabelled step any child of the kind (mache-91d903).
func (w *ASTWalker) Query(root any, selector string) ([]Match, error) {
	ar, ok := root.(ASTRoot)
	if !ok {
		return nil, fmt.Errorf("ASTWalker.Query: expected ASTRoot, got %T", root)
	}
	if ar.SourceID == "" {
		return nil, fmt.Errorf("ASTWalker.Query: ASTRoot.SourceID is empty — a query is keyed by the file it runs in")
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

	// The file's section, loaded once (mache-4f3840): every node lookup below
	// answers from it.
	idx, err := w.fileIndex(ar.SourceID)
	if err != nil {
		return nil, fmt.Errorf("find %s nodes: %w", pattern.outerKind, err)
	}
	// Read source content for byte-range extraction — from the file's
	// section (fileSource), NOT a raw readSource. Query runs once per
	// schema selector per file, so an uncached read re-fetched+decompressed
	// the full file content ~N-selectors times per file; on a whole-repo
	// projection that was the dominant cost (mache-4f3840).
	source := w.fileSource(ar.SourceID)

	scopePath := pattern.innerScopePath()
	var matches []Match
	for _, unit := range idx.scopeUnits(idx.nodesByKind(ar.ParentPrefix, pattern.outerKind), scopePath) {
		values, captureRanges, ok := idx.resolveCaptures(pattern, scopePath, unit, source)
		if !ok || !pattern.accepts(values) {
			continue
		}
		matches = append(matches, &astMatch{
			values:        values,
			captureRanges: captureRanges,
			ctx: ASTRoot{
				DB:       ar.DB,
				SourceID: ar.SourceID,
				// Scope nested schema-child queries to the resolved @scope node
				// (which may be an inner node like type_spec), mirroring
				// SitterWalker.Context() returning the captured scope node.
				ParentPrefix: unit.scope.id,
			},
			startByte: unit.scope.startByte,
			endByte:   unit.scope.endByte,
			w:         w,
		})
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

// PackageName implements FileMeta — the file's Go package, read from the
// file's section ("" for non-Go).
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
