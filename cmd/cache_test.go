// Cache subcommand tests — bead mache-aeb262 Phase 1 + 2.
//
// Round-trip: build a synthetic mache.db with a known _source set,
// `mache cache push` to emit chunks + lockfile, then
// `mache cache pull` to restore into a fresh db. Assert the restored
// _source rows are byte-equal to the originals.

package cmd

import (
	"bytes"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	capnp "capnproto.org/go/capnp/v3"
	cache "github.com/agentic-research/ley-line-open/clients/go/leyline-schema/cache"
	"github.com/zeebo/blake3"
	_ "modernc.org/sqlite"
)

// synthSource is one row to seed into a synthetic mache.db's _source.
type synthSource struct {
	id, path, language string
	content            []byte
}

// makeSyntheticDB creates a SQLite database at `dbPath` with a
// _source table populated from `rows`. Mirrors what mache's ingest
// pipeline produces, but skips _ast / nodes etc. — cache push only
// reads _source, so other tables aren't needed.
func makeSyntheticDB(t *testing.T, dbPath string, rows []synthSource) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open synthetic db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TABLE _source (
		id TEXT PRIMARY KEY,
		path TEXT,
		language TEXT,
		content BLOB
	)`); err != nil {
		t.Fatalf("create _source: %v", err)
	}
	stmt, err := db.Prepare("INSERT INTO _source(id, path, language, content) VALUES(?,?,?,?)")
	if err != nil {
		t.Fatalf("prepare insert: %v", err)
	}
	defer func() { _ = stmt.Close() }()
	for _, r := range rows {
		if _, err := stmt.Exec(r.id, r.path, r.language, r.content); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}
}

// readBackSources returns the _source rows from a restored db in
// path order. Used to compare against the input rows.
func readBackSources(t *testing.T, dbPath string) []synthSource {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open restored db: %v", err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query("SELECT id, path, language, content FROM _source ORDER BY path")
	if err != nil {
		t.Fatalf("query restored: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []synthSource
	for rows.Next() {
		var r synthSource
		var pathN, langN sql.NullString
		var content []byte
		if err := rows.Scan(&r.id, &pathN, &langN, &content); err != nil {
			t.Fatalf("scan: %v", err)
		}
		r.path = pathN.String
		r.language = langN.String
		r.content = content
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

// ── push smoke ────────────────────────────────────────────────────

// Phase 1 happy path: build a db, push, assert the lockfile + chunks
// land at the documented paths with correct hashes.
func TestCachePush_EmitsLockfileAndChunks(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "input.db")
	outDir := filepath.Join(tmp, "out")

	rows := []synthSource{
		{id: "src/auth.go", path: "src/auth.go", language: "go", content: []byte("package auth\n\nfunc Validate(s string) error { return nil }\n")},
		{id: "src/main.go", path: "src/main.go", language: "go", content: []byte("package main\n\nfunc main() {}\n")},
	}
	makeSyntheticDB(t, dbPath, rows)

	var buf bytes.Buffer
	if err := runCachePush(&buf, dbPath, outDir); err != nil {
		t.Fatalf("push: %v\n%s", err, buf.String())
	}

	// Lockfile bytes (canonical .bin)
	binPath := filepath.Join(outDir, "mache.lock.bin")
	lfBytes, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read lockfile bin: %v", err)
	}
	msg, err := capnp.Unmarshal(lfBytes)
	if err != nil {
		t.Fatalf("unmarshal lockfile: %v", err)
	}
	lf, err := cache.ReadRootCacheLockfile(msg)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}

	// Meta sanity
	m, _ := lf.Meta()
	if got, _ := m.Producer(); got != "mache" {
		t.Errorf("Meta.Producer: want mache, got %q", got)
	}
	if got, _ := m.SchemaVersion(); got != CacheVersion {
		t.Errorf("Meta.SchemaVersion: want %q, got %q", CacheVersion, got)
	}
	procs, _ := m.InputProcessors()
	if procs.Len() != 1 {
		t.Errorf("Meta.InputProcessors: want 1, got %d", procs.Len())
	}

	// Sources count
	srcs, _ := lf.Sources()
	if srcs.Len() != len(rows) {
		t.Fatalf("Sources: want %d, got %d", len(rows), srcs.Len())
	}

	// Each chunk file lives at <outDir>/objects/<hash[0..2]>/<hash[2..]>
	// and round-trip-hashes to the lockfile's claim.
	for i := 0; i < srcs.Len(); i++ {
		s := srcs.At(i)
		path, _ := s.Path()
		kind, _ := s.Kind()
		if kind != "go-source" {
			t.Errorf("source[%d] kind: want go-source, got %q", i, kind)
		}
		chunkHash, _ := s.ChunkHash()
		chunkBytes, _ := chunkHash.Bytes()
		if len(chunkBytes) != 32 {
			t.Fatalf("source[%d] chunkHash len: want 32, got %d", i, len(chunkBytes))
		}
		chunkPath := filepath.Join(
			outDir, "objects",
			hex.EncodeToString(chunkBytes[:1]),
			hex.EncodeToString(chunkBytes[1:]),
		)
		body, err := os.ReadFile(chunkPath)
		if err != nil {
			t.Fatalf("read chunk for %s at %s: %v", path, chunkPath, err)
		}
		actual := blake3.Sum256(body)
		if !bytes.Equal(actual[:], chunkBytes) {
			t.Errorf("chunk drift for %s: file hashes to %x, lockfile says %x", path, actual, chunkBytes)
		}
	}

	// TOML rendering
	tomlPath := filepath.Join(outDir, "mache.lock.toml")
	if _, err := os.Stat(tomlPath); err != nil {
		t.Errorf("mache.lock.toml missing: %v", err)
	}
}

// Push refuses to emit an empty lockfile (empty db is a producer
// bug — the catch is cheap and prevents shipping a useless cache).
func TestCachePush_RefusesEmptyDB(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "empty.db")
	outDir := filepath.Join(tmp, "out")
	makeSyntheticDB(t, dbPath, nil)

	var buf bytes.Buffer
	err := runCachePush(&buf, dbPath, outDir)
	if err == nil {
		t.Fatalf("push on empty db should fail; got nil error\n%s", buf.String())
	}
}

// ── push + pull round-trip ────────────────────────────────────────

// Phase 1 + 2 end-to-end: db → push → pull → db. Restored content
// must be byte-equal to original.
func TestCachePushPull_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "input.db")
	outDir := filepath.Join(tmp, "out")
	restoredPath := filepath.Join(tmp, "restored.db")

	original := []synthSource{
		{id: "src/auth.go", path: "src/auth.go", language: "go", content: []byte("package auth\n\nfunc Validate(s string) error { return nil }\n")},
		{id: "src/main.go", path: "src/main.go", language: "go", content: []byte("package main\n\nfunc main() {}\n")},
		{id: "src/types.go", path: "src/types.go", language: "go", content: []byte("package main\n\ntype X struct{}\n")},
	}
	makeSyntheticDB(t, dbPath, original)

	var pushBuf bytes.Buffer
	if err := runCachePush(&pushBuf, dbPath, outDir); err != nil {
		t.Fatalf("push: %v\n%s", err, pushBuf.String())
	}

	var pullBuf bytes.Buffer
	if err := runCachePull(&pullBuf, outDir, restoredPath, true); err != nil {
		t.Fatalf("pull: %v\n%s", err, pullBuf.String())
	}

	restored := readBackSources(t, restoredPath)
	if len(restored) != len(original) {
		t.Fatalf("restored row count: want %d, got %d", len(original), len(restored))
	}

	// Paths + languages + content must all round-trip. We sort by
	// path on both sides (push reads ORDER BY id, pull writes in
	// lockfile order; both rebuilds re-sort by path on read).
	pathToOrig := map[string]synthSource{}
	for _, r := range original {
		pathToOrig[r.path] = r
	}
	for _, r := range restored {
		want, ok := pathToOrig[r.path]
		if !ok {
			t.Errorf("restored has unexpected path %q", r.path)
			continue
		}
		if !bytes.Equal(r.content, want.content) {
			t.Errorf("content drift for %s: want %q, got %q", r.path, want.content, r.content)
		}
		if r.language != want.language {
			t.Errorf("language drift for %s: want %q, got %q", r.path, want.language, r.language)
		}
	}
}

// Pull rejects a lockfile whose schemaVersion doesn't match what
// this mache knows. Bumps require explicit code review; silent
// version skew is exactly what schemaVersion prevents.
func TestCachePull_RejectsWrongSchemaVersion(t *testing.T) {
	tmp := t.TempDir()
	outDir := filepath.Join(tmp, "out")
	restoredPath := filepath.Join(tmp, "restored.db")
	if err := os.MkdirAll(filepath.Join(outDir, "objects", "00"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Build a fresh lockfile with the WRONG schemaVersion. Doing this
	// directly via the capnp builder is cleaner than mutating a real
	// push's output (mutating an Unmarshal'd message has subtle
	// re-serialization behavior).
	msg, seg, err := capnp.NewMessage(capnp.SingleSegment(nil))
	if err != nil {
		t.Fatalf("new message: %v", err)
	}
	lf, err := cache.NewRootCacheLockfile(seg)
	if err != nil {
		t.Fatalf("new lockfile: %v", err)
	}
	m, _ := lf.NewMeta()
	_ = m.SetProducer("mache")
	_ = m.SetSchemaVersion("9.9.9-incompatible")
	// At least one source so the lockfile isn't trivially-empty.
	srcs, _ := lf.NewSources(1)
	s := srcs.At(0)
	_ = s.SetPath("a")
	_ = s.SetKind("go-source")
	ih, _ := s.NewInputHash()
	chBytes := blake3.Sum256([]byte("a"))
	_ = ih.SetBytes(chBytes[:])
	ch, _ := s.NewChunkHash()
	_ = ch.SetBytes(chBytes[:])
	// Also need the chunk on disk so pull doesn't 404 before checking version.
	chunkPath := filepath.Join(outDir, "objects", hex.EncodeToString(chBytes[:1]), hex.EncodeToString(chBytes[1:]))
	if err := os.MkdirAll(filepath.Dir(chunkPath), 0o755); err != nil {
		t.Fatalf("mkdir bucket: %v", err)
	}
	if err := os.WriteFile(chunkPath, []byte("a"), 0o644); err != nil {
		t.Fatalf("write chunk: %v", err)
	}
	// Root = BLAKE3(chunkHash) per ADR-0021 mache rule.
	rh := blake3.New()
	_, _ = rh.Write(chBytes[:])
	rootBytes := rh.Sum(nil)
	r, _ := lf.NewRoot()
	_ = r.SetBytes(rootBytes)
	out, _ := msg.Marshal()
	binPath := filepath.Join(outDir, "mache.lock.bin")
	if err := os.WriteFile(binPath, out, 0o644); err != nil {
		t.Fatalf("write lockfile: %v", err)
	}

	err = runCachePull(new(bytes.Buffer), outDir, restoredPath, true)
	if err == nil {
		t.Fatalf("pull should refuse mismatched schemaVersion; got nil error")
	}
}

// Pull rejects a lockfile whose chunk on disk has wrong bytes (the
// verify-on-read substrate guarantee). Tampering with a chunk file
// must surface as a hard fail, not silent restoration.
func TestCachePull_VerifyRejectsTamperedChunk(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "input.db")
	outDir := filepath.Join(tmp, "out")
	restoredPath := filepath.Join(tmp, "restored.db")

	makeSyntheticDB(t, dbPath, []synthSource{
		{id: "x", path: "x", language: "go", content: []byte("original content")},
	})
	if err := runCachePush(new(bytes.Buffer), dbPath, outDir); err != nil {
		t.Fatalf("push: %v", err)
	}

	// Tamper: overwrite the first chunk file with junk.
	chunksDir := filepath.Join(outDir, "objects")
	bucketDirs, _ := os.ReadDir(chunksDir)
	if len(bucketDirs) == 0 {
		t.Fatal("no bucket dirs found")
	}
	bucket := filepath.Join(chunksDir, bucketDirs[0].Name())
	files, _ := os.ReadDir(bucket)
	if len(files) == 0 {
		t.Fatal("no chunk files found")
	}
	tamperPath := filepath.Join(bucket, files[0].Name())
	if err := os.WriteFile(tamperPath, []byte("tampered"), 0o644); err != nil {
		t.Fatalf("tamper write: %v", err)
	}

	err := runCachePull(new(bytes.Buffer), outDir, restoredPath, true)
	if err == nil {
		t.Fatalf("pull --verify should refuse tampered chunk; got nil error")
	}
}

// Pull with --verify=false skips the chunk-hash check (faster for
// trusted local restores). Tampered chunks DO restore but with
// wrong content; this test pins that the flag actually disables
// verification (so callers know what they're opting out of).
func TestCachePull_NoVerifyAcceptsTamperedChunk(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "input.db")
	outDir := filepath.Join(tmp, "out")
	restoredPath := filepath.Join(tmp, "restored.db")

	makeSyntheticDB(t, dbPath, []synthSource{
		{id: "x", path: "x", language: "go", content: []byte("original content")},
	})
	if err := runCachePush(new(bytes.Buffer), dbPath, outDir); err != nil {
		t.Fatalf("push: %v", err)
	}

	// Tamper
	chunksDir := filepath.Join(outDir, "objects")
	bucketDirs, _ := os.ReadDir(chunksDir)
	bucket := filepath.Join(chunksDir, bucketDirs[0].Name())
	files, _ := os.ReadDir(bucket)
	tamperPath := filepath.Join(bucket, files[0].Name())
	_ = os.WriteFile(tamperPath, []byte("tampered"), 0o644)

	// With verify=false, restore succeeds but content is the tampered bytes.
	if err := runCachePull(new(bytes.Buffer), outDir, restoredPath, false); err != nil {
		t.Fatalf("pull --verify=false should accept; got %v", err)
	}

	restored := readBackSources(t, restoredPath)
	if len(restored) != 1 {
		t.Fatalf("restored row count: want 1, got %d", len(restored))
	}
	if !bytes.Equal(restored[0].content, []byte("tampered")) {
		t.Errorf("--verify=false should have produced tampered bytes, got %q", restored[0].content)
	}
}

// Idempotent push: running push twice with the same input produces
// the same chunk files (and the second run doesn't error on existing
// files). Pins the (IM) axiom at the consumer surface.
func TestCachePush_Idempotent(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "input.db")
	outDir := filepath.Join(tmp, "out")
	makeSyntheticDB(t, dbPath, []synthSource{
		{id: "a", path: "a.go", language: "go", content: []byte("package a\n")},
	})

	if err := runCachePush(new(bytes.Buffer), dbPath, outDir); err != nil {
		t.Fatalf("first push: %v", err)
	}
	// Snapshot chunk files.
	chunksDir := filepath.Join(outDir, "objects")
	type snap struct {
		name string
		size int64
	}
	beforeBuckets, _ := os.ReadDir(chunksDir)
	var before []snap
	for _, bd := range beforeBuckets {
		bucket := filepath.Join(chunksDir, bd.Name())
		files, _ := os.ReadDir(bucket)
		for _, f := range files {
			info, _ := f.Info()
			before = append(before, snap{name: f.Name(), size: info.Size()})
		}
	}

	// Second push.
	if err := runCachePush(new(bytes.Buffer), dbPath, outDir); err != nil {
		t.Fatalf("second push: %v", err)
	}
	afterBuckets, _ := os.ReadDir(chunksDir)
	var after []snap
	for _, bd := range afterBuckets {
		bucket := filepath.Join(chunksDir, bd.Name())
		files, _ := os.ReadDir(bucket)
		for _, f := range files {
			info, _ := f.Info()
			after = append(after, snap{name: f.Name(), size: info.Size()})
		}
	}

	if len(before) != len(after) {
		t.Errorf("chunk count drift after idempotent push: %d → %d", len(before), len(after))
	}
	for i := range before {
		if i >= len(after) || before[i] != after[i] {
			t.Errorf("chunk drift at index %d: before=%+v after=%+v", i, before[i], after[i])
		}
	}
}
