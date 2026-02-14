# Kern Integration: NFS Backend for Mache

> Audit and design document for replacing mache's FUSE mount layer with kern's NFSv3-on-localhost approach.

## 1. FUSE Audit

### 1.1 Files Importing FUSE

| File | Import | Purpose |
|------|--------|---------|
| `internal/fs/root.go` | `github.com/winfsp/cgofuse/fuse` | Core FUSE filesystem (`MacheFS` struct) |
| `internal/fs/cgo_darwin.go` | `"C"` (fuse-t framework headers) | macOS CGO linker flags |
| `internal/fs/root_test.go` | `github.com/winfsp/cgofuse/fuse` | Unit tests for FUSE operations |
| `cmd/mount.go` | `github.com/winfsp/cgofuse/fuse` | Mount orchestration (`FileSystemHost`) |
| `go.mod` | `github.com/winfsp/cgofuse v1.6.0` | Dependency |

CGO bridge (`internal/fs/cgo_darwin.go`):
```go
//go:build darwin
/*
#cgo CFLAGS: -I/Library/Frameworks/fuse_t.framework/Versions/Current/Headers
#cgo LDFLAGS: -F/Library/Frameworks -framework fuse_t -Wl,-rpath,/Library/Frameworks
*/
import "C"
```

### 1.2 FUSE Operations Implemented

`MacheFS` embeds `fuse.FileSystemBase` and overrides:

#### Read Path

| Operation | What it does |
|-----------|-------------|
| `Getattr(path, stat, fh)` | Populates inode/mode/size/timestamps. Delegates to `Graph.GetNode()`. Handles root, `_schema.json`, `.query/*`, graph nodes. |
| `Open(path, flags)` | Validates file existence. Read-only returns `(0, 0)`. Write opens allocate a `writeHandle`. |
| `Read(path, buff, ofst, fh)` | Reads from writeHandle buffer if dirty, else from `_schema.json` or `Graph.ReadContent()`. |
| `Opendir(path)` | Calls `Graph.ListChildren()`, caches entries in `dirHandle` map keyed by handle ID. |
| `Readdir(path, fill, ofst, fh)` | Auto-mode: iterates all cached entries at offset=0. Populates inline stats for ReaddirPlus. |
| `Releasedir(path, fh)` | Frees cached `dirHandle`. |
| `Readlink(path)` | `.query/` symlink results only. Returns relative symlink target. |

#### Write Path

| Operation | What it does |
|-----------|-------------|
| `Write(path, buff, ofst, fh)` | Writes to `writeHandle.buf` or `queryWriteHandle.buf`. |
| `Truncate(path, size, fh)` | Resizes `writeHandle.buf`. |
| `Release(path, fh)` | **The commit point.** Query handles: execute SQL. Write handles: `Splice()` -> `goimports` -> re-ingest -> `Graph.Invalidate()`. |
| `Flush(path, fh)` | No-op. |
| `Create(path, flags, mode)` | Only under `.query/`. Creates `queryWriteHandle` for `ctl` files. |
| `Mkdir(path, mode)` | Only under `.query/`. Creates query result entry. |
| `Unlink(path)` | `.query/`: error. Graph nodes: removes source file or splices empty content. |
| `Rmdir(path)` | Only under `.query/`. Deletes query result entry. |

#### Stubs (no-op, return 0)

`Utimens`, `Chmod`, `Chown`

### 1.3 Splice / Write-Back Engine

**File:** `internal/writeback/splice.go`

Single exported function:
```go
func Splice(origin graph.SourceOrigin, newContent []byte) error
```

Where `SourceOrigin` is:
```go
type SourceOrigin struct {
    FilePath  string
    StartByte uint32
    EndByte   uint32
}
```

Algorithm: reads source file, validates byte range, constructs `src[:start] + newContent + src[end:]`, atomic write via temp file + rename.

**Write-back pipeline** (triggered in `MacheFS.Release`):
1. `writeback.Splice(*node.Origin, wh.buf)` — byte-range replacement
2. `exec.Command("goimports", "-w", node.Origin.FilePath).Run()` — auto-format
3. `fs.Engine.Ingest(node.Origin.FilePath)` — re-ingest to update graph
4. `fs.Graph.Invalidate(wh.nodeID)` — evict cached content

**Delete pipeline** (in `MacheFS.Unlink`):
- Whole-file nodes: `os.Remove` source file, re-ingest parent
- Partial nodes (code blocks): splice empty content, goimports, re-ingest

### 1.4 Mount Lifecycle

**File:** `cmd/mount.go`

Startup sequence in `rootCmd.RunE`:
1. Resolve schema/data paths
2. Load or infer schema (`api.Topology`)
3. Create graph backend: `.db` -> `SQLiteGraph` with `EagerScan()`; non-`.db` -> `MemoryStore` + `Engine.Ingest()`
4. Create `MacheFS`: `machefs.NewMacheFS(schema, g)`
5. Wire query support: `macheFs.SetQueryFunc(...)` (type-switches on graph backend)
6. Wire write-back (if `--writable` + engine non-nil)
7. Create FUSE host: `fuse.NewFileSystemHost(macheFs)`
8. `host.SetCapReaddirPlus(true)`
9. `host.Mount(mountPoint, opts)` — **blocking** (enters FUSE event loop)

Shutdown: `host.Mount()` blocks until unmount. cgofuse handles SIGINT/SIGTERM internally. Deferred `Close()` calls run after Mount returns.

**No explicit signal handler in mache code** — cgofuse owns this.

### 1.5 FUSE-Specific Features

**Inode generation** (`pathIno`): FNV-1a hash of full path. Root = 1.
```go
func pathIno(path string) uint64
```

**Mount options** (from `cmd/mount.go`):
```
uid, gid, fsname=mache, subtype=mache
entry_timeout=0.0, attr_timeout=0.0, negative_timeout=0.0
direct_io
nobrowse (macOS), noattrcache (macOS)
ro (unless --writable)
```

**Handle maps** (3, all protected by `handleMu sync.Mutex`):
- `handles map[uint64]*dirHandle` — directory listings
- `writeHandles map[uint64]*writeHandle` — write buffers
- `queryWriteHandles map[uint64]*queryWriteHandle` — SQL query write buffers

**Timestamps**: all set to `mountTime` except nodes with non-zero `ModTime`.

**Virtual files**:
- `/_schema.json` — serialized schema (read-only)
- `/.query/` — Plan 9-style query directory (when `SetQueryFunc()` called)

**ReaddirPlus**: enabled via `host.SetCapReaddirPlus(true)`, entries include inline stat.

---

## 2. Operation Mapping: FUSE -> billy.Filesystem

### 2.1 Read Path

| FUSE Operation | billy Equivalent | Notes |
|---|---|---|
| `Getattr(path)` | `Stat(path)` / `Lstat(path)` | billy returns `os.FileInfo`. NFS GETATTR needs size, mode, times. Direct mapping. |
| `Opendir(path)` | `ReadDir(path)` | billy `ReadDir` returns `[]os.FileInfo` in one call — no handle/release dance needed. |
| `Readdir(path, fill, ofst, fh)` | `ReadDir(path)` | go-nfs handles the NFS READDIR/READDIRPLUS -> billy mapping internally. |
| `Releasedir(path, fh)` | *(none)* | No equivalent needed — billy is stateless for dirs. |
| `Open(path, flags)` | `Open(path)` / `OpenFile(path, flag, perm)` | billy returns `billy.File` which supports `Read`, `ReadAt`, `Seek`. |
| `Read(path, buf, ofst, fh)` | `file.ReadAt(buf, ofst)` | go-nfs translates NFS READ offset/count into billy File operations. |
| `Readlink(path)` | `Readlink(path)` | Direct mapping. Only used for `.query/` symlinks. |

### 2.2 Write Path

| FUSE Operation | billy Equivalent | Notes |
|---|---|---|
| `Write(path, buf, ofst, fh)` | `file.Write(buf)` / `file.WriteAt(buf, ofst)` | billy.File supports Write. **But:** mache buffers writes in-memory and commits on Release. billy has no Release. Need a hook. |
| `Create(path, flags, mode)` | `Create(path)` / `OpenFile(path, O_CREATE, mode)` | Direct mapping. |
| `Truncate(path, size, fh)` | `file.Truncate(size)` | billy.File has Truncate. |
| `Release(path, fh)` | `file.Close()` | **Critical:** mache's commit point. In billy, `Close()` is the equivalent. The billy adapter must trigger splice on Close. |
| `Mkdir(path, mode)` | `MkdirAll(path, mode)` | billy only has MkdirAll, not Mkdir. Acceptable. |
| `Unlink(path)` | `Remove(path)` | Direct mapping. Adapter triggers splice/delete. |
| `Rmdir(path)` | `Remove(path)` | billy uses Remove for both files and dirs. |

### 2.3 Stubs / No Equivalent

| FUSE Operation | billy Equivalent | Notes |
|---|---|---|
| `Utimens` | `Chtimes(path, atime, mtime)` | Only in `billy.Change` (optional). Can stub. |
| `Chmod` | `Chmod(path, mode)` | Only in `billy.Change`. Can stub. |
| `Chown` | `Lchown(path, uid, gid)` | Only in `billy.Change`. Can stub. |
| `Flush` | *(none)* | No-op in mache already. |

### 2.4 billy Methods With No FUSE Equivalent

| billy Method | Needed? | Strategy |
|---|---|---|
| `Rename(old, new)` | No | Return `billy.ErrNotSupported` or `EROFS` |
| `TempFile(dir, prefix)` | No | Return `billy.ErrNotSupported` |
| `Symlink(target, link)` | Maybe | Only for `.query/` results. Can stub initially. |
| `Chroot(path)` | Maybe | go-nfs may call this. Use `helper/chroot` wrapper from go-billy. |
| `Root()` | Yes | Return `"/"` |
| `Join(elem...)` | Yes | `filepath.Join` |

### 2.5 Key Difference: Stateless vs Handle-Based

FUSE is handle-based: `Open` returns a handle, `Read/Write/Release` use it. billy is mostly stateless: `Open` returns a `billy.File` which is a self-contained `io.ReadWriteCloser` with Seek.

go-nfs bridges this gap internally with `CachingHandler` (LRU of path -> UUID file handles). Mache's adapter doesn't need handle management — go-nfs does it.

**Exception:** The write-commit-on-Release pattern. In FUSE, mache accumulates writes in a buffer and commits on `Release`. In billy, the equivalent is: accumulate writes in the `billy.File` implementation's internal buffer, commit on `Close()`. The `kernfs.File.Close()` method must trigger the splice pipeline.

---

## 3. Integration Design

### 3.1 Library or Binary?

**Library.** kern is ~150 lines. Import as a Go package into mache.

But: kern doesn't need to become a library *itself*. The two reusable pieces are:
1. **go-nfs** (`github.com/willscott/go-nfs`) — already a library
2. **billy.Filesystem adapter for mache's Graph** — this is new code

The adapter lives in **mache**, not kern. kern proved the concept; mache takes the dependencies directly.

Proposed package API (in mache):

```go
// internal/nfsmount/server.go
package nfsmount

// Serve starts an NFS server on the given port backed by the billy filesystem.
// Blocks until the listener is closed.
func Serve(fs billy.Filesystem, port int) error

// Mount calls `mount -t nfs` to mount the NFS server at the given mountpoint.
// Requires root on macOS (uses sudo). Returns when mount is established.
func Mount(port int, mountpoint string, writable bool) error

// Unmount calls `umount` on the mountpoint.
func Unmount(mountpoint string) error
```

And the adapter:

```go
// internal/nfsmount/graphfs.go
package nfsmount

// GraphFS adapts mache's graph.Graph interface to billy.Filesystem.
type GraphFS struct { ... }

func NewGraphFS(g graph.Graph, schema *api.Topology) *GraphFS
```

### 3.2 billy Adapter: GraphFS

The adapter wraps `graph.Graph` to satisfy `billy.Filesystem` (16 methods).

**Thin wrapper, not significant translation.** Mache's Graph already provides exactly what billy needs:

| billy method | Graph method | Translation |
|---|---|---|
| `Stat(path)` | `GetNode(path)` | `Node` -> `os.FileInfo` (mode, size, modTime) |
| `Open(path)` | `GetNode(path)` | Return `graphFile{node, graph, 0}` |
| `ReadDir(path)` | `ListChildren(path)` + `GetNode` per child | Build `[]os.FileInfo` |
| `OpenFile(path, flag, perm)` | `GetNode(path)` | Flag-aware: O_RDONLY -> graphFile, O_RDWR -> writeFile |

The `billy.File` implementation for reads:
```go
type graphFile struct {
    node   *graph.Node
    graph  graph.Graph
    nodeID string
    pos    int64
}

func (f *graphFile) Read(p []byte) (int, error)   { /* ReadContent at f.pos */ }
func (f *graphFile) ReadAt(p []byte, off int64) (int, error) { /* ReadContent at off */ }
func (f *graphFile) Seek(offset int64, whence int) (int64, error) { /* update f.pos */ }
func (f *graphFile) Close() error { return nil }
func (f *graphFile) Name() string { return f.nodeID }
```

**Virtual files** (`_schema.json`, `.query/`) must be handled in GraphFS, not in the graph backend. These are presentation-layer concerns that currently live in `MacheFS`. They move into `GraphFS`.

**Capability advertisement:**
```go
func (g *GraphFS) Capabilities() billy.Capability {
    caps := billy.ReadCapability | billy.SeekCapability
    if g.writable {
        caps |= billy.WriteCapability
    }
    return caps
}
```

### 3.3 Write Path

Write RPCs flow: NFS WRITE -> go-nfs -> `billy.File.Write()` -> buffer -> `billy.File.Close()` -> splice

The `writeFile` implementation:
```go
type writeFile struct {
    graphFile
    buf      []byte
    dirty    bool
    origin   *graph.SourceOrigin
    onClose  func(origin graph.SourceOrigin, content []byte) error  // splice callback
}

func (f *writeFile) Write(p []byte) (int, error) { /* append to buf, mark dirty */ }
func (f *writeFile) Close() error {
    if f.dirty && f.onClose != nil {
        return f.onClose(*f.origin, f.buf)
    }
    return nil
}
```

The `onClose` callback wires to the same pipeline as today's `MacheFS.Release`:
1. `writeback.Splice(origin, content)`
2. `goimports -w` (if Go file)
3. `engine.Ingest(origin.FilePath)`
4. `graph.Invalidate(nodeID)`

**Gap:** NFS WRITE RPCs arrive as individual writes. go-nfs calls `billy.File.Write()` for each. The NFS client may issue multiple WRITEs before CLOSE (COMMIT in NFS). go-nfs translates NFS COMMIT into... nothing in billy (there's no `Sync` method). The `Close()` is the commit point, same as FUSE Release.

**Gap:** NFS has no `UNLINK` -> splice path in go-nfs by default. `billy.Remove()` is called directly. The adapter's `Remove()` must implement the same logic as `MacheFS.Unlink` (check for Origin, splice empty or remove file).

### 3.4 Mount Lifecycle

```
mache mount /mnt/data --backend=nfs   (or just default)

1. Schema + graph construction (unchanged — cmd/mount.go steps 1-6)
2. Create GraphFS adapter: nfsmount.NewGraphFS(g, schema)
3. Wire write-back callback (if --writable)
4. Pick a free port (net.Listen ":0")
5. Start NFS server in goroutine: go nfsmount.Serve(graphFs, port)
6. Mount: nfsmount.Mount(port, mountpoint, writable)
7. Block on signal (SIGINT/SIGTERM)
8. On signal: Unmount(mountpoint), close listener, deferred Close() calls
```

**sudo question:** `mount -t nfs` requires root on macOS. Options:
- **Option A:** mache calls `sudo mount ...` — prompts for password. This is what kern does.
- **Option B:** mache itself runs as root — undesirable.
- **Option C:** Use `diskutil mount` or other unprivileged mount path — doesn't exist for NFS on macOS.

**Recommendation:** Option A. Print the mount command, exec `sudo mount`. Same pattern as kern. The NFS server runs unprivileged; only the mount syscall needs root.

**Unmount:** `sudo umount mountpoint`. On macOS, `diskutil unmount mountpoint` works without sudo for user-mounted NFS. Test both.

### 3.5 Fallback

**Keep FUSE as `--backend=fuse`.** Reasons:
- Containers (Docker) often lack NFS client kernel module
- CI environments (GitHub Actions with libfuse-dev) already work with FUSE
- Linux FUSE has no fuse-t penalty — it's native kernel FUSE
- Gradual migration: users can switch back if NFS has issues

The `--backend` flag defaults to `nfs` on macOS, `fuse` on Linux (where FUSE is native and fine). Can revisit when NFS is proven on Linux too.

### 3.6 Testing

Current tests in `internal/fs/root_test.go` test FUSE operations directly (call `Getattr`, `Opendir`, `Readdir`, `Read` on `MacheFS`).

**Strategy: test the projection, not the mount mechanism.**

1. **GraphFS unit tests** — test the billy adapter directly. Create a `MemoryStore`, populate it, wrap in `GraphFS`, call billy methods. No mount needed.
2. **Integration tests** — start NFS server on random port, mount, verify with `ls`/`cat`/`stat`. Gated behind `MACHE_TEST_NFS_MOUNT=1` (needs sudo).
3. **Existing FUSE tests** — keep as-is for `--backend=fuse`. They test MacheFS which remains unchanged.
4. **Backend-agnostic tests** — factor out a test suite that takes a `graph.Graph` and verifies projection correctness. Run against both GraphFS (billy) and MacheFS (FUSE). This is the ideal end state but not required for the first PR.

---

## 4. Migration Plan

### PR 1: GraphFS adapter (no mount changes)

**What:** `internal/nfsmount/graphfs.go` — billy.Filesystem backed by graph.Graph. Read-only.

**Files:**
- `internal/nfsmount/graphfs.go` — adapter implementation
- `internal/nfsmount/graphfs_test.go` — unit tests against MemoryStore

**Proves:** mache's graph can satisfy billy.Filesystem. No CLI changes. No new dependencies besides go-billy.

**Size:** ~200-300 lines of Go + tests.

### PR 2: NFS server + mount

**What:** `internal/nfsmount/server.go` — Serve/Mount/Unmount. Wire into `cmd/mount.go` behind `--backend=nfs`.

**Files:**
- `internal/nfsmount/server.go` — NFS server wrapper
- `cmd/mount.go` — add `--backend` flag, NFS path
- `go.mod` — add `willscott/go-nfs`, `go-git/go-billy/v5`

**Proves:** `mache mount /mnt/data --backend=nfs` works. Read-only. Can `ls`, `cat`, `grep` the projected tree.

### PR 3: Write path

**What:** Wire splice engine through billy.File.Close().

**Files:**
- `internal/nfsmount/graphfs.go` — add writeFile, Remove with splice
- `cmd/mount.go` — wire write callback for NFS backend

**Proves:** `mache mount --writable --backend=nfs` works. Edits flow back to source files.

### PR 4: .query/ support

**What:** Virtual query directory in GraphFS.

**Files:**
- `internal/nfsmount/graphfs.go` — add .query/ handling (Create, Mkdir, symlink results)

**Proves:** Full feature parity with FUSE backend.

### PR 5: Default backend switch

**What:** Make NFS the default on macOS. `--backend=fuse` still works.

**Files:**
- `cmd/mount.go` — change default
- `README.md` — update install instructions (no more fuse-t dependency on macOS)

### What gets deleted (eventually)

- `internal/fs/root.go` — MacheFS (FUSE implementation)
- `internal/fs/cgo_darwin.go` — CGO bridge for fuse-t
- `internal/fs/root_test.go` — FUSE-specific tests
- `go.mod` — remove `github.com/winfsp/cgofuse`
- Build: remove fuse-t CGO flags from Taskfile.yml
- Install: remove fuse-t framework dependency on macOS

**Not yet.** Keep FUSE until NFS is proven stable. Delete in a future major version.

---

## 5. Open Questions

### 5.1 Where does kernfs live?

Kern's LLM is building `internal/kernfs/` in the kern repo — a billy.Filesystem backed by an in-memory tree. Mache needs a billy.Filesystem backed by `graph.Graph`. These could be:

- **Option A:** Mache builds its own `GraphFS` adapter (proposed above). kern's kernfs is irrelevant — it's an intermediate step.
- **Option B:** kern's kernfs becomes the shared adapter. Mache populates it from Graph, kern serves it over NFS. More indirection, less clear benefit.
- **Option C:** kernfs evolves a `BackingStore` interface that graph.Graph satisfies. kernfs handles billy plumbing, Graph provides data.

**Recommendation:** Option A for now. GraphFS in mache is thin (~200 lines) and tightly coupled to Graph semantics. If kern stabilizes kernfs with a clean `BackingStore` interface, mache can adopt it later. Don't couple the repos prematurely.

### 5.2 sudo for mount

macOS requires root for `mount -t nfs`. Options:
- Prompt via `sudo` (kern's approach) — works but annoying
- Use `osascript` for GUI password prompt — fragile
- Ship a small suid helper — security concern
- Use `nfs.conf` to allow unprivileged mounts — not a thing on macOS

**Likely answer:** `sudo mount` with a clear message. Unmount via `diskutil unmount` (no sudo needed on macOS for user-initiated NFS mounts, needs verification).

### 5.3 Port management

Kern hardcodes port 11111. Mache needs:
- Multiple concurrent mounts (different schemas/data)
- No port conflicts
- Deterministic enough for reconnect after restart

**Proposal:** Use ephemeral ports (`net.Listen(":0")`). Write the port to a state file (`~/.mache/mounts/{mountpoint-hash}.json`) for unmount/reconnect.

### 5.4 NFS cache behavior vs direct_io

Current FUSE config uses `direct_io` + zero timeouts — every read hits userspace. This is correct for writable mounts where content changes after splice.

NFS with `locallocks` gets kernel page cache for free. For read-only mounts this is pure win. For writable mounts, after a splice, the NFS client's cached pages are stale.

**Options:**
- Short attribute timeouts (e.g., 1s) — NFS client re-validates frequently
- Server-initiated cache invalidation — NFSv3 has no mechanism for this (NFSv4 does via delegations, but go-nfs is v3 only)
- Accept staleness — after write-back, subsequent reads may show old content until cache expires. For mache's use case (AI agents editing code), this may be acceptable with a 1-5s window.

### 5.5 Linux support

NFS-on-localhost works on Linux too, but:
- `locallocks` is macOS-specific. Linux equivalent: `local_lock=all` mount option
- Linux has native FUSE (no fuse-t penalty) — NFS is less clearly better
- Linux containers may not have `mount` capability or NFS client modules

**Default:** NFS on macOS, FUSE on Linux. Revisit when Linux NFS is tested.

### 5.6 Coordination with kern repo

kern is building kernfs (billy adapter) and will evolve its NFS server code. Should mache:
- Import kern as a Go module? (couples the repos)
- Copy the NFS serve/mount/unmount pattern? (duplication but independence)
- Factor shared code into a third module? (premature)

**Recommendation:** Copy the pattern (it's ~30 lines for Serve + Mount + Unmount). Take `willscott/go-nfs` and `go-git/go-billy/v5` as direct dependencies. If kern evolves a reusable `kernfs` package worth importing, do it then.

### 5.7 billy.File.Lock/Unlock

billy.File has `Lock()` and `Unlock()` methods. With `locallocks`, the kernel handles locking and never sends NLM RPCs to go-nfs. So go-nfs never calls billy Lock/Unlock. These can be no-ops.

But: if someone mounts without `locallocks`, go-nfs doesn't handle NLM either (it's a separate protocol). Locks simply won't work. Document this: `locallocks` (macOS) or `local_lock=all` (Linux) is **required**.

### 5.8 _schema.json and .query/ ownership

Currently these virtual files live in `MacheFS` (FUSE layer). With NFS, they move to `GraphFS` (billy adapter). This is the right place — they're presentation concerns, not graph data.

But `.query/` is complex: it involves creating directories, writing to `ctl` files, reading symlink results. This is ~100 lines of stateful logic in MacheFS. It needs careful porting.

**Alternative:** Factor `.query/` into its own package that both MacheFS and GraphFS can use. This enables backend-agnostic testing of query behavior.
