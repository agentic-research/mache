package fs

import (
	"database/sql"
	"encoding/json"
	"hash/fnv"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/writeback"
	"github.com/winfsp/cgofuse/fuse"
)

// pathIno returns a stable inode number for a given path.
// Root gets inode 1 (FUSE convention). All others use FNV-1a hash.
func pathIno(path string) uint64 {
	if path == "/" {
		return 1
	}
	h := fnv.New64a()
	h.Write([]byte(path))
	ino := h.Sum64()
	if ino <= 1 {
		ino = 2 // 0 = unknown, 1 = root — both reserved
	}
	return ino
}

// dirHandle caches a directory listing and its path for stat population.
// Readdir uses the path to construct full FUSE paths for GetNode calls,
// enabling ReaddirPlus (stats returned inline, eliminating N+1 LOOKUP calls).
type dirHandle struct {
	path    string   // FUSE directory path (e.g., "/vulns")
	entries []string // base names: [".", "..", "child1", ...]
}

// writeHandle tracks an in-progress write to a file node.
type writeHandle struct {
	path   string // FUSE path (used to resolve node ID)
	nodeID string // graph node ID
	buf    []byte
	dirty  bool
}

// queryWriteHandle tracks an in-progress write to a .query/ file.
type queryWriteHandle struct {
	name string // query name (base of /.query/<name>)
	buf  []byte
}

// queryEntry stores one result row from a query execution.
type queryEntry struct {
	name   string // display name: path with "/" → "_"
	target string // symlink target: "../../" + original_path
}

// queryResult stores executed query results.
type queryResult struct {
	sql     string
	entries []queryEntry
}

// MacheFS implements the FUSE interface from cgofuse.
// It delegates all file/directory decisions to the Graph — no heuristics.
type MacheFS struct {
	fuse.FileSystemBase
	Schema    *api.Topology
	Graph     graph.Graph
	mountTime fuse.Timespec

	// Virtual _schema.json content (serialized once at mount time)
	schemaJSON []byte

	// Write-back support (nil Engine = read-only)
	Writable bool
	Engine   *ingest.Engine

	// Directory handle cache: Opendir builds the entry list once,
	// Readdir slices from it, Releasedir frees it.
	handleMu          sync.Mutex
	handles           map[uint64]*dirHandle        // fh → directory listing + path
	writeHandles      map[uint64]*writeHandle      // fh → write buffer
	queryWriteHandles map[uint64]*queryWriteHandle // fh → query write buffer
	nextHandle        uint64

	// Query directory support (nil queryFn = feature disabled)
	queryFn func(string, ...any) (*sql.Rows, error)
	queryMu sync.RWMutex
	queries map[string]*queryResult
}

func NewMacheFS(schema *api.Topology, g graph.Graph) *MacheFS {
	sj, _ := json.MarshalIndent(schema, "", "  ")
	sj = append(sj, '\n')
	return &MacheFS{
		Schema:            schema,
		Graph:             g,
		schemaJSON:        sj,
		mountTime:         fuse.NewTimespec(time.Now()),
		handles:           make(map[uint64]*dirHandle),
		writeHandles:      make(map[uint64]*writeHandle),
		queryWriteHandles: make(map[uint64]*queryWriteHandle),
	}
}

// SetQueryFunc enables the /.query/ magic directory. Pass the SQLiteGraph's
// QueryRefs method. If never called, /.query is not exposed.
func (fs *MacheFS) SetQueryFunc(fn func(string, ...any) (*sql.Rows, error)) {
	fs.queryFn = fn
	fs.queries = make(map[string]*queryResult)
}

// isQueryPath returns true if the path is under /.query.
func (fs *MacheFS) isQueryPath(path string) bool {
	return fs.queryFn != nil && (path == "/.query" || strings.HasPrefix(path, "/.query/"))
}

// Open validates that the path is a file node. For writable mounts,
// write flags allocate a writeHandle backed by the node's current content.
func (fs *MacheFS) Open(path string, flags int) (int, uint64) {
	if path == "/_schema.json" {
		if flags&(syscall.O_WRONLY|syscall.O_RDWR) != 0 {
			return -fuse.EACCES, 0
		}
		return 0, 0
	}

	if fs.isQueryPath(path) {
		if path == "/.query" {
			return -fuse.EISDIR, 0
		}

		parts := strings.SplitN(strings.TrimPrefix(path, "/.query/"), "/", 2)
		name := parts[0]

		if len(parts) == 1 {
			// /.query/<name>
			fs.queryMu.RLock()
			_, ok := fs.queries[name]
			fs.queryMu.RUnlock()
			if ok {
				return -fuse.EISDIR, 0
			}
			return -fuse.ENOENT, 0
		}

		// /.query/<name>/<entry>
		entry := parts[1]
		if entry == "ctl" {
			writing := flags&(syscall.O_WRONLY|syscall.O_RDWR) != 0
			if writing {
				// Allocate write handle for query
				fs.handleMu.Lock()
				fh := fs.nextHandle
				fs.nextHandle++
				fs.queryWriteHandles[fh] = &queryWriteHandle{name: name}
				fs.handleMu.Unlock()
				return 0, fh
			}
			return 0, 0 // Read-only open of ctl (maybe allow reading SQL back?)
		}

		// Symlinks (results) -> Open should probably fail or be handled?
		// FUSE usually uses Readlink. Open on a symlink depends on O_NOFOLLOW.
		// If we return 0, we claim it's a file.
		return -fuse.ENOENT, 0
	}

	node, err := fs.Graph.GetNode(path)
	if err != nil {
		return -fuse.ENOENT, 0
	}
	if node.Mode.IsDir() {
		return -fuse.EISDIR, 0
	}

	writing := flags&(syscall.O_WRONLY|syscall.O_RDWR) != 0
	if writing {
		if !fs.Writable || node.Origin == nil {
			return -fuse.EACCES, 0
		}

		// Pre-fill buffer with existing content (for O_RDWR / partial writes)
		var buf []byte
		if flags&syscall.O_TRUNC == 0 {
			size := node.ContentSize()
			if size > 0 {
				buf = make([]byte, size)
				n, _ := fs.Graph.ReadContent(path, buf, 0)
				buf = buf[:n]
			}
		}

		fs.handleMu.Lock()
		fh := fs.nextHandle
		fs.nextHandle++
		fs.writeHandles[fh] = &writeHandle{
			path:   path,
			nodeID: node.ID,
			buf:    buf,
			dirty:  false,
		}
		fs.handleMu.Unlock()
		return 0, fh
	}

	return 0, 0
}

// Getattr trusts the node's declared Mode.
func (fs *MacheFS) Getattr(path string, stat *fuse.Stat_t, fh uint64) int {
	stat.Atim = fs.mountTime
	stat.Mtim = fs.mountTime
	stat.Ctim = fs.mountTime
	stat.Birthtim = fs.mountTime

	stat.Ino = pathIno(path)

	if path == "/" {
		stat.Mode = fuse.S_IFDIR | 0o555
		stat.Nlink = 2
		return 0
	}

	if path == "/_schema.json" {
		stat.Mode = fuse.S_IFREG | 0o444
		stat.Nlink = 1
		stat.Size = int64(len(fs.schemaJSON))
		return 0
	}

	if fs.isQueryPath(path) {
		return fs.queryGetattr(path, stat)
	}

	node, err := fs.Graph.GetNode(path)
	if err != nil {
		return -fuse.ENOENT
	}

	if node.Mode.IsDir() {
		stat.Mode = fuse.S_IFDIR | 0o555
		stat.Nlink = 2
	} else {
		perm := uint32(0o444)
		if fs.Writable && node.Origin != nil {
			perm = 0o644
		}
		stat.Mode = fuse.S_IFREG | perm
		stat.Nlink = 1
		stat.Size = node.ContentSize()
	}

	if !node.ModTime.IsZero() {
		stat.Mtim = fuse.NewTimespec(node.ModTime)
		stat.Ctim = fuse.NewTimespec(node.ModTime)
	}

	return 0
}

// Opendir fetches the directory listing once and caches it by handle.
func (fs *MacheFS) Opendir(path string) (int, uint64) {
	if fs.isQueryPath(path) {
		return fs.queryOpendir(path)
	}

	if path != "/" {
		node, err := fs.Graph.GetNode(path)
		if err != nil {
			return -fuse.ENOENT, 0
		}
		if !node.Mode.IsDir() {
			return -fuse.ENOTDIR, 0
		}
	}

	children, err := fs.Graph.ListChildren(path)
	if err != nil {
		return -fuse.ENOENT, 0
	}

	entries := make([]string, 0, len(children)+4)
	entries = append(entries, ".", "..")
	if path == "/" {
		entries = append(entries, "_schema.json")
		if fs.queryFn != nil {
			entries = append(entries, ".query")
		}
	}
	for _, c := range children {
		entries = append(entries, filepath.Base(c))
	}

	fs.handleMu.Lock()
	fh := fs.nextHandle
	fs.nextHandle++
	fs.handles[fh] = &dirHandle{path: path, entries: entries}
	fs.handleMu.Unlock()

	return 0, fh
}

// Releasedir frees the cached directory listing.
func (fs *MacheFS) Releasedir(path string, fh uint64) int {
	fs.handleMu.Lock()
	delete(fs.handles, fh)
	fs.handleMu.Unlock()
	return 0
}

// Readdir serves entries from the cached handle with inline stats (ReaddirPlus).
// Auto-mode (offset=0 to fill): fuse-t requires all results in the first pass.
// The NFS translation layer handles pagination to the macOS NFS client.
// cgofuse fill() convention: true = accepted, false = buffer full.
//
// Stats are populated for each entry to enable ReaddirPlus — this eliminates
// the N+1 LOOKUP storm where the kernel would issue a separate Getattr call
// for every directory entry. With fuse-t's NFS translation, this maps to
// NFS READDIRPLUS which returns attributes inline.
func (fs *MacheFS) Readdir(path string, fill func(name string, stat *fuse.Stat_t, ofst int64) bool, ofst int64, fh uint64) int {
	fs.handleMu.Lock()
	dh, ok := fs.handles[fh]
	fs.handleMu.Unlock()

	if !ok {
		// Fallback: no handle (shouldn't happen, but be safe).
		// Uses nil stats — the kernel falls back to individual LOOKUPs.
		children, err := fs.Graph.ListChildren(path)
		if err != nil {
			return -fuse.ENOENT
		}
		entries := make([]string, 0, len(children)+4)
		entries = append(entries, ".", "..")
		if path == "/" {
			entries = append(entries, "_schema.json")
			if fs.queryFn != nil {
				entries = append(entries, ".query")
			}
		}
		for _, c := range children {
			entries = append(entries, filepath.Base(c))
		}
		for _, name := range entries {
			if !fill(name, nil, 0) {
				break
			}
		}
		return 0
	}

	// Auto-mode: pass offset=0 to fill(). FUSE handles pagination internally.
	// fuse-t translates to NFS READDIR and manages cookie-based continuation.
	for _, name := range dh.entries {
		stat := fs.readdirStat(dh.path, name)
		if !fill(name, stat, 0) {
			break // buffer full
		}
	}

	return 0
}

// readdirStat builds a fuse.Stat_t for one directory entry.
// Returns nil for entries that can't be resolved (kernel falls back to LOOKUP).
func (fs *MacheFS) readdirStat(dirPath, name string) *fuse.Stat_t {
	stat := &fuse.Stat_t{
		Atim:     fs.mountTime,
		Mtim:     fs.mountTime,
		Ctim:     fs.mountTime,
		Birthtim: fs.mountTime,
	}

	switch name {
	case ".":
		stat.Ino = pathIno(dirPath)
		stat.Mode = fuse.S_IFDIR | 0o555
		stat.Nlink = 2
		return stat
	case "..":
		stat.Ino = pathIno(filepath.Dir(dirPath))
		stat.Mode = fuse.S_IFDIR | 0o555
		stat.Nlink = 2
		return stat
	}

	// Build full FUSE path for this entry
	var fullPath string
	if dirPath == "/" {
		fullPath = "/" + name
	} else {
		fullPath = dirPath + "/" + name
	}

	if name == "_schema.json" {
		stat.Ino = pathIno(fullPath)
		stat.Mode = fuse.S_IFREG | 0o444
		stat.Nlink = 1
		stat.Size = int64(len(fs.schemaJSON))
		return stat
	}

	if name == ".query" && fs.queryFn != nil {
		stat.Ino = pathIno(fullPath)
		stat.Mode = fuse.S_IFDIR | 0o777
		stat.Nlink = 2
		return stat
	}

	node, err := fs.Graph.GetNode(fullPath)
	if err != nil {
		return nil // fallback to individual LOOKUP
	}

	stat.Ino = pathIno(fullPath)
	if node.Mode.IsDir() {
		stat.Mode = fuse.S_IFDIR | 0o555
		stat.Nlink = 2
	} else {
		perm := uint32(0o444)
		if fs.Writable && node.Origin != nil {
			perm = 0o644
		}
		stat.Mode = fuse.S_IFREG | perm
		stat.Nlink = 1
		stat.Size = node.ContentSize()
	}
	return stat
}

// Read returns the Data of a file node.
// If a writeHandle exists for this fh, reads from the in-progress buffer instead.
func (fs *MacheFS) Read(path string, buff []byte, ofst int64, fh uint64) int {
	fs.handleMu.Lock()
	wh, isWrite := fs.writeHandles[fh]
	fs.handleMu.Unlock()

	if isWrite && wh.buf != nil {
		if ofst >= int64(len(wh.buf)) {
			return 0
		}
		n := copy(buff, wh.buf[ofst:])
		return n
	}

	if path == "/_schema.json" {
		if ofst >= int64(len(fs.schemaJSON)) {
			return 0
		}
		n := copy(buff, fs.schemaJSON[ofst:])
		return n
	}

	node, err := fs.Graph.GetNode(path)
	if err != nil {
		return -fuse.ENOENT
	}
	if node.Mode.IsDir() {
		return -fuse.EISDIR
	}

	n, err := fs.Graph.ReadContent(path, buff, ofst)
	if err != nil {
		return -fuse.EIO
	}
	return n
}

// Write appends/overwrites data in the writeHandle buffer.
func (fs *MacheFS) Write(path string, buff []byte, ofst int64, fh uint64) int {
	// Query write handle?
	fs.handleMu.Lock()
	qwh, isQuery := fs.queryWriteHandles[fh]
	fs.handleMu.Unlock()
	if isQuery {
		qwh.buf = append(qwh.buf, buff...)
		return len(buff)
	}

	fs.handleMu.Lock()
	wh, ok := fs.writeHandles[fh]
	fs.handleMu.Unlock()

	if !ok {
		return -fuse.EBADF
	}

	end := ofst + int64(len(buff))
	if end > int64(len(wh.buf)) {
		grown := make([]byte, end)
		copy(grown, wh.buf)
		wh.buf = grown
	}
	copy(wh.buf[ofst:], buff)
	wh.dirty = true
	return len(buff)
}

// Truncate resizes the writeHandle buffer (called via ftruncate).
func (fs *MacheFS) Truncate(path string, size int64, fh uint64) int {
	fs.handleMu.Lock()
	wh, ok := fs.writeHandles[fh]
	fs.handleMu.Unlock()

	if !ok {
		// No write handle — check if this is a writable node
		if !fs.Writable {
			return -fuse.EACCES
		}
		return 0
	}

	if size < int64(len(wh.buf)) {
		wh.buf = wh.buf[:size]
	} else if size > int64(len(wh.buf)) {
		grown := make([]byte, size)
		copy(grown, wh.buf)
		wh.buf = grown
	}
	wh.dirty = true
	return 0
}

// Flush is a no-op — we commit on Release.
func (fs *MacheFS) Flush(path string, fh uint64) int {
	return 0
}

// Release is THE COMMIT POINT for write-back.
// On close: splice new content into source → goimports → re-ingest → graph updated.
func (fs *MacheFS) Release(path string, fh uint64) int {
	// Query write handle? Execute the SQL on close.
	fs.handleMu.Lock()
	qwh, isQuery := fs.queryWriteHandles[fh]
	if isQuery {
		delete(fs.queryWriteHandles, fh)
	}
	fs.handleMu.Unlock()
	if isQuery {
		return fs.queryExecute(qwh)
	}

	fs.handleMu.Lock()
	wh, ok := fs.writeHandles[fh]
	if ok {
		delete(fs.writeHandles, fh)
	}
	fs.handleMu.Unlock()

	if !ok || !wh.dirty {
		return 0
	}

	node, err := fs.Graph.GetNode(wh.path)
	if err != nil {
		log.Printf("writeback: node %s not found: %v", wh.nodeID, err)
		return -fuse.EIO
	}
	if node.Origin == nil {
		return -fuse.EACCES
	}

	// 1. Splice new content into source file
	if err := writeback.Splice(*node.Origin, wh.buf); err != nil {
		log.Printf("writeback: splice failed for %s: %v", node.Origin.FilePath, err)
		return -fuse.EIO
	}

	// 2. Run goimports (failure-tolerant — if agent wrote broken code,
	//    we still want it in the FS so the agent can see and fix the error)
	if err := exec.Command("goimports", "-w", node.Origin.FilePath).Run(); err != nil {
		log.Printf("writeback: goimports failed for %s (continuing): %v", node.Origin.FilePath, err)
	}

	// 3. Re-ingest the source file — updates ALL Origins from this file
	if fs.Engine != nil {
		// Ensure timestamp changes for NFS cache invalidation (granularity safety)
		time.Sleep(2 * time.Millisecond)
		if err := fs.Engine.Ingest(node.Origin.FilePath); err != nil {
			log.Printf("writeback: re-ingest failed for %s: %v", node.Origin.FilePath, err)
		}
	}

	// 4. Invalidate cached size/content — the file's content changed,
	// so the size cache and content cache must be evicted to prevent
	// stale data on the next Getattr or Read.
	fs.Graph.Invalidate(wh.nodeID)

	return 0
}

// ---------------------------------------------------------------------------
// Magic /.query/ directory — Plan 9-style: write SQL, get symlink results
// ---------------------------------------------------------------------------

// Mkdir handles directory creation.

// We only allow Mkdir under /.query to create a new query container.

func (fs *MacheFS) Mkdir(path string, mode uint32) int {
	if !fs.isQueryPath(path) {
		return -fuse.EACCES
	}

	// Path must be /.query/<name>

	parts := strings.SplitN(strings.TrimPrefix(path, "/.query/"), "/", 2)

	if len(parts) != 1 || parts[0] == "" {
		return -fuse.EACCES
	}

	name := parts[0]

	fs.queryMu.Lock()

	if _, exists := fs.queries[name]; exists {

		fs.queryMu.Unlock()

		return -fuse.EEXIST

	}

	// Create empty query result

	fs.queries[name] = &queryResult{
		sql: "",

		entries: []queryEntry{},
	}

	fs.queryMu.Unlock()

	return 0
}

// queryGetattr handles Getattr for paths under /.query.

func (fs *MacheFS) queryGetattr(path string, stat *fuse.Stat_t) int {
	if path == "/.query" {

		stat.Mode = fuse.S_IFDIR | 0o777

		stat.Nlink = 2

		stat.Ino = pathIno(path)

		return 0

	}

	parts := strings.SplitN(strings.TrimPrefix(path, "/.query/"), "/", 2)

	name := parts[0]

	fs.queryMu.RLock()

	qr, ok := fs.queries[name]

	fs.queryMu.RUnlock()

	if !ok {
		return -fuse.ENOENT
	}

	if len(parts) == 1 {

		// /.query/<name> — result directory

		stat.Ino = pathIno(path)

		stat.Mode = fuse.S_IFDIR | 0o777

		stat.Nlink = 2

		return 0

	}

	// /.query/<name>/<entry>

	entryName := parts[1]

	// Special "ctl" file

	if entryName == "ctl" {

		stat.Ino = pathIno(path)

		stat.Mode = fuse.S_IFREG | 0o666

		stat.Nlink = 1

		stat.Size = int64(len(qr.sql)) // show current SQL size?

		return 0

	}

	// Result symlinks

	for _, e := range qr.entries {
		if e.name == entryName {

			stat.Ino = pathIno(path)

			stat.Mode = fuse.S_IFLNK | 0o777

			stat.Nlink = 1

			stat.Size = int64(len(e.target))

			return 0

		}
	}

	return -fuse.ENOENT
}

// queryOpendir handles Opendir for /.query and /.query/<name>.

func (fs *MacheFS) queryOpendir(path string) (int, uint64) {
	var entries []string

	if path == "/.query" {

		entries = []string{".", ".."}

		fs.queryMu.RLock()

		for name := range fs.queries {
			entries = append(entries, name)
		}

		fs.queryMu.RUnlock()

	} else {

		name := strings.TrimPrefix(path, "/.query/")

		if strings.Contains(name, "/") {
			return -fuse.ENOENT, 0
		}

		fs.queryMu.RLock()

		qr, ok := fs.queries[name]

		fs.queryMu.RUnlock()

		if !ok {
			return -fuse.ENOENT, 0
		}

		entries = make([]string, 0, len(qr.entries)+3)

		entries = append(entries, ".", "..", "ctl") // Always list ctl

		for _, e := range qr.entries {
			entries = append(entries, e.name)
		}

	}

	fs.handleMu.Lock()

	fh := fs.nextHandle

	fs.nextHandle++

	fs.handles[fh] = &dirHandle{path: path, entries: entries}

	fs.handleMu.Unlock()

	return 0, fh
}

// Create handles file creation under /.query — allocates a write handle

// for accumulating SQL. Also handles regular FUSE Create if needed.

func (fs *MacheFS) Create(path string, flags int, mode uint32) (int, uint64) {
	if !fs.isQueryPath(path) {
		return -fuse.EACCES, 0
	}

	// Must be /.query/<name>/ctl

	parts := strings.SplitN(strings.TrimPrefix(path, "/.query/"), "/", 2)

	if len(parts) != 2 {
		return -fuse.EACCES, 0
	}

	name := parts[0]

	entry := parts[1]

	if entry != "ctl" {
		return -fuse.EACCES, 0
	}

	// Ensure parent query directory exists

	fs.queryMu.RLock()

	_, ok := fs.queries[name]

	fs.queryMu.RUnlock()

	if !ok {
		return -fuse.ENOENT, 0
	}

	fs.handleMu.Lock()

	fh := fs.nextHandle

	fs.nextHandle++

	fs.queryWriteHandles[fh] = &queryWriteHandle{name: name}

	fs.handleMu.Unlock()

	return 0, fh
}

// queryExecute runs the SQL from a completed query write and stores results.
func (fs *MacheFS) queryExecute(qwh *queryWriteHandle) int {
	sqlStr := strings.TrimSpace(string(qwh.buf))
	if sqlStr == "" {
		return 0
	}

	// 1. Run SQL (No Lock held!)
	// Multiple agents can do this simultaneously
	rows, err := fs.queryFn(sqlStr)
	if err != nil {
		log.Printf("query: execute %q: %v", qwh.name, err)
		return -fuse.EIO
	}
	defer func() { _ = rows.Close() }() // safe to ignore

	var entries []queryEntry
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			continue
		}
		// Strip leading slash if present
		p = strings.TrimPrefix(p, "/")
		if p == "" {
			continue
		}
		entries = append(entries, queryEntry{
			name:   strings.ReplaceAll(p, "/", "_"),
			target: "../../" + p,
		})
	}
	if err := rows.Err(); err != nil {
		log.Printf("query: rows %q: %v", qwh.name, err)
	}

	// 2. Save Results (Microsecond Lock)
	fs.queryMu.Lock()
	fs.queries[qwh.name] = &queryResult{sql: sqlStr, entries: entries}
	fs.queryMu.Unlock()

	return 0
}

// Readlink returns the symlink target for /.query/<name>/<entry>.
func (fs *MacheFS) Readlink(path string) (int, string) {
	if !fs.isQueryPath(path) {
		return -fuse.EINVAL, ""
	}

	rel := strings.TrimPrefix(path, "/.query/")
	parts := strings.SplitN(rel, "/", 2)
	if len(parts) != 2 {
		return -fuse.EINVAL, ""
	}

	fs.queryMu.RLock()
	qr, ok := fs.queries[parts[0]]
	fs.queryMu.RUnlock()
	if !ok {
		return -fuse.ENOENT, ""
	}

	for _, e := range qr.entries {
		if e.name == parts[1] {
			return 0, e.target
		}
	}
	return -fuse.ENOENT, ""
}

// Unlink removes a stored query result (/.query/<name>) or a file node.
func (fs *MacheFS) Unlink(path string) int {
	if fs.isQueryPath(path) {
		parts := strings.SplitN(strings.TrimPrefix(path, "/.query/"), "/", 2)
		if len(parts) == 1 {
			// Attempting to unlink the query directory itself
			return -fuse.EISDIR
		}

		// Unlinking contents?
		// ctl: prevent deletion? or allow and do nothing?
		// symlinks: these are virtual, can't delete individual results.
		return -fuse.EACCES
	}

	// Regular file deletion
	if !fs.Writable {
		return -fuse.EACCES
	}

	node, err := fs.Graph.GetNode(path)
	if err != nil {
		return -fuse.ENOENT
	}
	if node.Mode.IsDir() {
		return -fuse.EISDIR
	}
	if node.Origin == nil {
		return -fuse.EACCES // Virtual node without origin
	}

	// Check if this node represents the whole file?
	isWholeFile := false
	if info, err := os.Stat(node.Origin.FilePath); err == nil {
		if node.Origin.StartByte == 0 && int64(node.Origin.EndByte) == info.Size() {
			isWholeFile = true
		}
	}

	if isWholeFile {
		if err := os.Remove(node.Origin.FilePath); err != nil {
			log.Printf("Unlink: failed to remove file %s: %v", node.Origin.FilePath, err)
			return -fuse.EIO
		}
	} else {
		// Splice with empty content to "delete" the code block
		if err := writeback.Splice(*node.Origin, []byte{}); err != nil {
			log.Printf("Unlink: splice failed for %s: %v", node.Origin.FilePath, err)
			return -fuse.EIO
		}
		// Run goimports if it's a Go file (cleanup newlines etc)
		if strings.HasSuffix(node.Origin.FilePath, ".go") {
			_ = exec.Command("goimports", "-w", node.Origin.FilePath).Run()
		}
	}

	// Re-ingest
	if fs.Engine != nil {
		if isWholeFile {
			// File is gone, ingest parent dir to update structure
			// This might be expensive, but necessary to remove the node from graph
			parent := filepath.Dir(node.Origin.FilePath)
			if err := fs.Engine.Ingest(parent); err != nil {
				log.Printf("Unlink: re-ingest parent %s failed: %v", parent, err)
			}
			// Also need to remove the node from memory if Ingest doesn't?
			// Ingest(parent) will re-scan.
		} else {
			if err := fs.Engine.Ingest(node.Origin.FilePath); err != nil {
				log.Printf("Unlink: re-ingest file %s failed: %v", node.Origin.FilePath, err)
			}
		}
	}

	fs.Graph.Invalidate(node.ID)
	return 0
}

// Rmdir removes a query directory (/.query/<name>).
func (fs *MacheFS) Rmdir(path string) int {
	if !fs.isQueryPath(path) {
		return -fuse.EACCES
	}

	parts := strings.SplitN(strings.TrimPrefix(path, "/.query/"), "/", 2)
	if len(parts) != 1 || parts[0] == "" {
		return -fuse.ENOTDIR
	}
	name := parts[0]

	fs.queryMu.Lock()
	_, ok := fs.queries[name]
	if ok {
		delete(fs.queries, name)
	}
	fs.queryMu.Unlock()

	if !ok {
		return -fuse.ENOENT
	}
	return 0
}

// Utimens stub
func (fs *MacheFS) Utimens(path string, tmsp []fuse.Timespec) int {
	return 0
}

// Chmod stub
func (fs *MacheFS) Chmod(path string, mode uint32) int {
	return 0
}

// Chown stub
func (fs *MacheFS) Chown(path string, uid, gid uint32) int {
	return 0
}
