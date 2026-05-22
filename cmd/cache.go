// Cache subcommand surface — bead `mache-aeb262` Phase 1 + 2.
//
// `mache push <out-dir>` walks a mache-built `.db`, emits per-source
// chunks + a `mache.lock.toml` lockfile per LLO ADR-0021. Phase 2
// (`mache pull --from-local`) restores the db state from a lockfile
// + chunks.
//
// The lockfile schema is LLO's `cache.capnp` (substrate ADR-0021).
// On disk the rendering is TOML for diff-friendliness; the canonical
// bytes are also written as `mache.lock.bin` so the cross-runtime
// fixture suite can re-verify byte-equal against LLO's canonical
// encoding. Both files are written; the canonical .bin is the
// authoritative source.
//
// Producer namespace: `"mache"` (per ADR-0020).
// Kind vocabulary: `"go-source"`, `"rust-source"`, etc. — one per
// language. Matches mache's existing `_source.language` column.

package cmd

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	capnp "capnproto.org/go/capnp/v3"
	"github.com/BurntSushi/toml"
	cache "github.com/agentic-research/ley-line-open/clients/go/leyline-schema/cache"
	"github.com/spf13/cobra"
	"github.com/zeebo/blake3"
	_ "modernc.org/sqlite"
)

// CacheVersion is the schema_version mache writes into the lockfile
// meta. Bumps require an LLO ADR-0021 schema change first.
const CacheVersion = "0.1.0"

// MacheProducerName is the producer field per ADR-0020. Short-name
// convention for v1; reverse-DNS reserved for v2 if collisions.
const MacheProducerName = "mache"

// MacheProducerVersion is the mache version recorded in the lockfile.
// Bumped on mache release; pulled from build/version info at compile
// time when wired in. For now: a static placeholder; the actual
// value isn't load-bearing for the v1 verify path.
//
// TODO: thread the real version (cmd.Version) once the cobra root
// binding is in scope here.
const MacheProducerVersion = "0.x.y"

// Flag values (subcommand-scoped) — package-level so test code can
// override + restore cleanly.
var (
	cachePushDBPath string
	cachePushOutDir string
	cachePushRemote string
	cachePushScope  string
	cachePushTag    string
	cachePushToken  string
	cachePullInPath string
	cachePullOutDB  string
	cachePullVerify bool
	cachePullRemote string
	cachePullScope  string
	cachePullRef    string
	cachePullToken  string
)

var cacheCmd = &cobra.Command{
	Use:   "cache",
	Short: "Portable mache db: push/pull lockfile + chunks via the LLO cache substrate",
	Long: `Cache commands for the portable mache db feature (bead mache-aeb262).

Subcommands:
  push   Emit a lockfile + per-source chunks from a built .db
  pull   Restore a db state from a lockfile + chunks in a local CAS

Lockfile schema: LLO ADR-0021 (cache.capnp). On disk as both
mache.lock.toml (diff-friendly) and mache.lock.bin (canonical capnp
wire bytes). Phase 3 adds remote build-cache transport per
cloister-spec/build-cache/v1.
`,
}

var cachePushCmd = &cobra.Command{
	Use:   "push <out-dir>",
	Short: "Emit a lockfile + chunks from a mache .db (Phase 1+3)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cachePushOutDir = args[0]
		if cachePushDBPath == "" {
			return fmt.Errorf("--db is required")
		}
		if err := runCachePush(cmd.OutOrStdout(), cachePushDBPath, cachePushOutDir); err != nil {
			return err
		}
		if cachePushRemote != "" {
			if cachePushScope == "" {
				return fmt.Errorf("--scope is required when --remote is set")
			}
			token := cachePushToken
			if token == "" {
				token = os.Getenv("MACHE_CACHE_TOKEN")
			}
			return runCacheRemotePush(cmd.Context(), cmd.OutOrStdout(),
				cachePushOutDir, cachePushRemote, MacheProducerName, cachePushScope,
				cachePushTag, token)
		}
		return nil
	},
}

var cachePullCmd = &cobra.Command{
	Use:   "pull <in-dir>",
	Short: "Restore a .db state from a lockfile + chunks (Phase 2+3)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cachePullInPath = args[0]
		if cachePullOutDB == "" {
			return fmt.Errorf("--out-db is required")
		}
		if cachePullRemote != "" {
			if cachePullScope == "" {
				return fmt.Errorf("--scope is required when --remote is set")
			}
			token := cachePullToken
			if token == "" {
				token = os.Getenv("MACHE_CACHE_TOKEN")
			}
			if err := runCacheRemotePull(cmd.Context(), cmd.OutOrStdout(),
				cachePullRemote, MacheProducerName, cachePullScope, cachePullRef,
				token, cachePullInPath); err != nil {
				return err
			}
		}
		return runCachePull(cmd.OutOrStdout(), cachePullInPath, cachePullOutDB, cachePullVerify)
	},
}

func init() {
	cachePushCmd.Flags().StringVar(&cachePushDBPath, "db", "", "path to mache-built .db")
	_ = cachePushCmd.MarkFlagRequired("db")
	cachePushCmd.Flags().StringVar(&cachePushRemote, "remote", "", "Phase 3: OCI registry base URL")
	cachePushCmd.Flags().StringVar(&cachePushScope, "scope", "", "Phase 3: scope segment (e.g. <repo>/<commit>)")
	cachePushCmd.Flags().StringVar(&cachePushTag, "tag", "latest", "Phase 3: mutable tag")
	cachePushCmd.Flags().StringVar(&cachePushToken, "token", "", "Phase 3: bearer token (or MACHE_CACHE_TOKEN env)")

	cachePullCmd.Flags().StringVar(&cachePullOutDB, "out-db", "", "path to write the restored .db")
	_ = cachePullCmd.MarkFlagRequired("out-db")
	cachePullCmd.Flags().BoolVar(&cachePullVerify, "verify", true,
		"after restore, re-walk sources and assert their BLAKE3 matches the lockfile")
	cachePullCmd.Flags().StringVar(&cachePullRemote, "remote", "", "Phase 3: OCI registry base URL")
	cachePullCmd.Flags().StringVar(&cachePullScope, "scope", "", "Phase 3: scope segment")
	cachePullCmd.Flags().StringVar(&cachePullRef, "ref", "latest", "Phase 3: digest or tag")
	cachePullCmd.Flags().StringVar(&cachePullToken, "token", "", "Phase 3: bearer token (or MACHE_CACHE_TOKEN env)")

	cacheCmd.AddCommand(cachePushCmd)
	cacheCmd.AddCommand(cachePullCmd)
	rootCmd.AddCommand(cacheCmd)
}

// ─────────────────────────────────────────────────────────────────────
// Push (Phase 1): db → lockfile + chunks
// ─────────────────────────────────────────────────────────────────────

// sourceRow is one row of mache's `_source` table — the per-file
// metadata + content mache's ingest pipeline writes. We only read
// what we need.
type sourceRow struct {
	id       string
	path     string
	language string
	content  []byte
}

// chunkEntry pairs a source with its hashes. Built once per source,
// then converted into both the on-disk chunk file AND the lockfile's
// SourceEntry. Keeping them as a single struct avoids two passes
// over the db.
type chunkEntry struct {
	src        sourceRow
	inputHash  [32]byte // BLAKE3 of src.content (pre-processor)
	chunkHash  [32]byte // BLAKE3 of the emitted chunk bytes (v1: == inputHash)
	chunkBytes []byte   // What gets written to disk under <out>/objects/...
	fileName   string   // chunk file name (hex-encoded chunk hash)
}

func runCachePush(out io.Writer, dbPath, outDir string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", outDir, err)
	}
	chunksDir := filepath.Join(outDir, "objects")
	if err := os.MkdirAll(chunksDir, 0o755); err != nil {
		return fmt.Errorf("mkdir chunks: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", dbPath, err)
	}
	defer func() { _ = db.Close() }()

	sources, err := readSources(db)
	if err != nil {
		return fmt.Errorf("read _source: %w", err)
	}
	if len(sources) == 0 {
		return fmt.Errorf("db %s has no _source rows; refusing to emit an empty lockfile", dbPath)
	}

	entries := make([]chunkEntry, 0, len(sources))
	for _, s := range sources {
		// Phase 1 chunk = raw source content. Phase 4 will switch chunks
		// to be the per-source capnp-encoded parse output (the actual
		// "derived" cache content); for v1 the content-equals-chunk path
		// proves the lockfile + transport machinery end-to-end.
		ih := blake3.Sum256(s.content)
		ch := ih // v1: chunk == input bytes
		entries = append(entries, chunkEntry{
			src:        s,
			inputHash:  ih,
			chunkHash:  ch,
			chunkBytes: s.content,
			fileName:   hex.EncodeToString(ch[:]),
		})
	}

	// Write chunks. Use a content-addressed sub-layout under objects/
	// matching LLO's FsBlobStore convention (`<hash[0..2]>/<hash[2..]>`),
	// so a future migration to call FsBlobStore directly is a no-op.
	for _, e := range entries {
		bucket := filepath.Join(chunksDir, hex.EncodeToString(e.chunkHash[:1]))
		if err := os.MkdirAll(bucket, 0o755); err != nil {
			return fmt.Errorf("mkdir bucket %s: %w", bucket, err)
		}
		path := filepath.Join(bucket, hex.EncodeToString(e.chunkHash[1:]))
		// Idempotent: skip if present + content matches. If present
		// + mismatched, that's substrate corruption — fail loudly.
		if existing, err := os.ReadFile(path); err == nil {
			actual := blake3.Sum256(existing)
			if actual != e.chunkHash {
				return fmt.Errorf("chunk %s on disk has wrong hash %x (want %x)",
					path, actual, e.chunkHash)
			}
			continue
		}
		if err := writeFileAtomic(path, e.chunkBytes); err != nil {
			return fmt.Errorf("write chunk %s: %w", path, err)
		}
	}

	// Build the lockfile via capnp Builder.
	rootHash := computeRoot(entries)
	lfBytes, err := buildLockfile(entries, rootHash)
	if err != nil {
		return fmt.Errorf("build lockfile: %w", err)
	}

	// Write both renderings: canonical .bin (authoritative) + TOML
	// (diff-friendly). Producer commits both; consumers can pick.
	binPath := filepath.Join(outDir, "mache.lock.bin")
	if err := writeFileAtomic(binPath, lfBytes); err != nil {
		return fmt.Errorf("write lockfile bin: %w", err)
	}
	tomlPath := filepath.Join(outDir, "mache.lock.toml")
	if err := writeLockfileTOML(tomlPath, entries, rootHash); err != nil {
		return fmt.Errorf("write lockfile toml: %w", err)
	}

	_, _ = fmt.Fprintf(out, "wrote %d chunks to %s\n", len(entries), chunksDir)
	_, _ = fmt.Fprintf(out, "wrote %s (%d bytes canonical)\n", binPath, len(lfBytes))
	_, _ = fmt.Fprintf(out, "wrote %s (TOML rendering)\n", tomlPath)
	_, _ = fmt.Fprintf(out, "lockfile root: %x\n", rootHash)
	return nil
}

func readSources(db *sql.DB) ([]sourceRow, error) {
	// _source schema (per mache/internal/ingest/ast_walker.go):
	//   id TEXT PRIMARY KEY, content BLOB, path TEXT, language TEXT, ...
	// We select what we need; missing columns (older schemas) get NULL.
	rows, err := db.Query("SELECT id, path, language, content FROM _source ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []sourceRow
	for rows.Next() {
		var r sourceRow
		var pathN, langN sql.NullString
		var content []byte
		if err := rows.Scan(&r.id, &pathN, &langN, &content); err != nil {
			return nil, err
		}
		r.path = pathN.String
		r.language = langN.String
		r.content = content
		// Skip sources with no content AND no path — they can't be
		// reconstructed from a chunk. This shouldn't happen on a
		// well-formed mache db but the gate is cheap.
		if len(content) == 0 && r.path == "" {
			continue
		}
		// If content is empty but path is set, the original was a
		// path-only reference — load content from disk so the chunk
		// is self-contained.
		if len(content) == 0 {
			body, err := os.ReadFile(r.path)
			if err != nil {
				return nil, fmt.Errorf("load source content from %s: %w", r.path, err)
			}
			r.content = body
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// computeRoot derives the lockfile's `root` per ADR-0021's
// consumer-defined semantics. mache's rule: BLAKE3 of concatenated
// chunkHashes in source-id order. Matches what the conformance
// vectors at cloister-spec/build-cache/v1/vectors/ commit.
func computeRoot(entries []chunkEntry) [32]byte {
	h := blake3.New()
	for _, e := range entries {
		_, _ = h.Write(e.chunkHash[:])
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func buildLockfile(entries []chunkEntry, root [32]byte) ([]byte, error) {
	msg, seg, err := capnp.NewMessage(capnp.SingleSegment(nil))
	if err != nil {
		return nil, fmt.Errorf("new capnp message: %w", err)
	}
	lf, err := cache.NewRootCacheLockfile(seg)
	if err != nil {
		return nil, fmt.Errorf("new CacheLockfile: %w", err)
	}

	// Meta
	m, err := lf.NewMeta()
	if err != nil {
		return nil, fmt.Errorf("new meta: %w", err)
	}
	if err := m.SetProducer(MacheProducerName); err != nil {
		return nil, err
	}
	if err := m.SetProducerVersion(MacheProducerVersion); err != nil {
		return nil, err
	}
	if err := m.SetSchemaVersion(CacheVersion); err != nil {
		return nil, err
	}
	m.SetGeneratedAtMs(uint64(time.Now().UnixMilli()))

	// Processors — mache uses BLAKE3 for hashing; per-language parsers
	// not in scope for v1 (chunks are raw content).
	procs, err := m.NewInputProcessors(1)
	if err != nil {
		return nil, err
	}
	p := procs.At(0)
	if err := p.SetKind("blake3"); err != nil {
		return nil, err
	}
	if err := p.SetVersion("1.5.0"); err != nil {
		return nil, err
	}

	// Sources
	srcs, err := lf.NewSources(int32(len(entries)))
	if err != nil {
		return nil, fmt.Errorf("new sources: %w", err)
	}
	for i, e := range entries {
		s := srcs.At(i)
		if err := s.SetPath(e.src.path); err != nil {
			return nil, err
		}
		kind := e.src.language
		if kind == "" {
			kind = "unknown"
		} else {
			kind += "-source" // per ADR-0020 mache vocabulary
		}
		if err := s.SetKind(kind); err != nil {
			return nil, err
		}
		ih, err := s.NewInputHash()
		if err != nil {
			return nil, err
		}
		if err := ih.SetBytes(e.inputHash[:]); err != nil {
			return nil, err
		}
		ch, err := s.NewChunkHash()
		if err != nil {
			return nil, err
		}
		if err := ch.SetBytes(e.chunkHash[:]); err != nil {
			return nil, err
		}
	}

	// Topology — v1 emits no edges; mache's sheaf-driven incremental
	// path (Phase 4) populates this from leyline-sheaf later.
	if _, err := lf.NewTopology(0); err != nil {
		return nil, err
	}

	// Root
	r, err := lf.NewRoot()
	if err != nil {
		return nil, err
	}
	if err := r.SetBytes(root[:]); err != nil {
		return nil, err
	}

	// Wire-encode for deterministic byte equality across runs in Go.
	// True canonical-form equality with the Rust producer's
	// `set_root_canonical` is a v1.1 follow-up; for v1 the Go-side
	// wire bytes round-trip cleanly through capnp.Unmarshal — the
	// `mache pull` consumer (and the test gates) verify field-by-
	// field equality, not byte equality with the Rust producer.
	return msg.Marshal()
}

// tomlLockfile is the on-disk TOML rendering of a CacheLockfile.
// Field tags lowercase the names per BurntSushi/toml conventions
// + ADR-0021 §"TOML on-disk" example shape.
type tomlLockfile struct {
	Meta     tomlMeta           `toml:"meta"`
	Sources  []tomlSource       `toml:"sources"`
	Topology []tomlTopologyEdge `toml:"topology"`
	Root     string             `toml:"root"` // "blake3:<hex>"
}

type tomlMeta struct {
	Producer        string          `toml:"producer"`
	ProducerVersion string          `toml:"producer_version"`
	SchemaVersion   string          `toml:"schema_version"`
	GeneratedAtMs   uint64          `toml:"generated_at_ms"`
	InputProcessors []tomlProcessor `toml:"input_processors"`
}

type tomlProcessor struct {
	Kind    string `toml:"kind"`
	Version string `toml:"version"`
}

type tomlSource struct {
	Path      string `toml:"path"`
	InputHash string `toml:"input_hash"` // "blake3:<hex>"
	ChunkHash string `toml:"chunk_hash"` // "blake3:<hex>"
	Kind      string `toml:"kind"`
}

type tomlTopologyEdge struct {
	From     string `toml:"from"`
	ToSource string `toml:"to_source"`
}

func writeLockfileTOML(path string, entries []chunkEntry, root [32]byte) error {
	lf := tomlLockfile{
		Meta: tomlMeta{
			Producer:        MacheProducerName,
			ProducerVersion: MacheProducerVersion,
			SchemaVersion:   CacheVersion,
			GeneratedAtMs:   uint64(time.Now().UnixMilli()),
			InputProcessors: []tomlProcessor{
				{Kind: "blake3", Version: "1.5.0"},
			},
		},
		Sources:  make([]tomlSource, 0, len(entries)),
		Topology: []tomlTopologyEdge{},
		Root:     "blake3:" + hex.EncodeToString(root[:]),
	}
	for _, e := range entries {
		kind := e.src.language
		if kind == "" {
			kind = "unknown"
		} else {
			kind += "-source"
		}
		lf.Sources = append(lf.Sources, tomlSource{
			Path:      e.src.path,
			InputHash: "blake3:" + hex.EncodeToString(e.inputHash[:]),
			ChunkHash: "blake3:" + hex.EncodeToString(e.chunkHash[:]),
			Kind:      kind,
		})
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-lockfile-*")
	if err != nil {
		return err
	}
	tmpName := f.Name()
	enc := toml.NewEncoder(f)
	if err := enc.Encode(&lf); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

// ─────────────────────────────────────────────────────────────────────
// Pull (Phase 2): lockfile + chunks → restored db state
// ─────────────────────────────────────────────────────────────────────

func runCachePull(out io.Writer, inDir, outDBPath string, verify bool) error {
	// Read the canonical lockfile (prefer .bin over .toml since .bin
	// is authoritative; .toml is for humans).
	binPath := filepath.Join(inDir, "mache.lock.bin")
	lfBytes, err := os.ReadFile(binPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", binPath, err)
	}
	msg, err := capnp.Unmarshal(lfBytes)
	if err != nil {
		return fmt.Errorf("unmarshal lockfile: %w", err)
	}
	lf, err := cache.ReadRootCacheLockfile(msg)
	if err != nil {
		return fmt.Errorf("read CacheLockfile root: %w", err)
	}

	meta, err := lf.Meta()
	if err != nil {
		return fmt.Errorf("read meta: %w", err)
	}
	if got, _ := meta.SchemaVersion(); got != CacheVersion {
		return fmt.Errorf("lockfile schemaVersion %q != mache supports %q (run mache push with the matching version)",
			got, CacheVersion)
	}
	prod, _ := meta.Producer()
	if prod != MacheProducerName {
		return fmt.Errorf("lockfile producer %q != mache (refusing to restore a foreign-producer bundle)", prod)
	}

	srcs, err := lf.Sources()
	if err != nil {
		return fmt.Errorf("read sources: %w", err)
	}

	// Open a fresh SQLite db; create _source schema matching mache's
	// ingest pipeline. v1 restores only _source (content + path);
	// derived tables (_ast, _lsp*) are out of scope for Phase 2 — a
	// subsequent re-ingest reproduces them from the restored content.
	if err := os.MkdirAll(filepath.Dir(outDBPath), 0o755); err != nil {
		return err
	}
	// Remove existing target so SQLite doesn't append to a stale file.
	_ = os.Remove(outDBPath)
	db, err := sql.Open("sqlite", outDBPath)
	if err != nil {
		return fmt.Errorf("open out db: %w", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TABLE _source (
		id TEXT PRIMARY KEY,
		path TEXT,
		language TEXT,
		content BLOB
	)`); err != nil {
		return fmt.Errorf("create _source: %w", err)
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare("INSERT INTO _source(id, path, language, content) VALUES(?,?,?,?)")
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer func() { _ = stmt.Close() }()

	chunksDir := filepath.Join(inDir, "objects")
	for i := 0; i < srcs.Len(); i++ {
		s := srcs.At(i)
		path, _ := s.Path()
		kindFull, _ := s.Kind()
		// Strip the "-source" suffix mache push added.
		language := kindFull
		if language != "" && language != "unknown" {
			const suffix = "-source"
			if len(language) > len(suffix) && language[len(language)-len(suffix):] == suffix {
				language = language[:len(language)-len(suffix)]
			}
		}
		chunkHashCommon, _ := s.ChunkHash()
		chunkHashBytes, _ := chunkHashCommon.Bytes()
		if len(chunkHashBytes) != 32 {
			_ = tx.Rollback()
			return fmt.Errorf("source[%d] chunkHash is %d bytes (want 32)", i, len(chunkHashBytes))
		}
		chunkPath := filepath.Join(
			chunksDir,
			hex.EncodeToString(chunkHashBytes[:1]),
			hex.EncodeToString(chunkHashBytes[1:]),
		)
		body, err := os.ReadFile(chunkPath)
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("read chunk %s: %w", chunkPath, err)
		}
		if verify {
			actual := blake3.Sum256(body)
			if actual != *(*[32]byte)(chunkHashBytes) {
				_ = tx.Rollback()
				return fmt.Errorf("chunk %s drift: claimed %x but bytes hash to %x",
					chunkPath, chunkHashBytes, actual)
			}
		}
		// Synthesize an `id` since the lockfile only commits `path`.
		// mache's ingest uses path as id when no other identifier is
		// supplied; mirror that here.
		id := path
		if id == "" {
			id = fmt.Sprintf("chunk_%d", i)
		}
		if _, err := stmt.Exec(id, path, language, body); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("insert source[%d]: %w", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	// Verify the root matches the chunk-hash chain (consumer-side
	// guarantee).
	if verify {
		var concatenated [][]byte
		for i := 0; i < srcs.Len(); i++ {
			ch, _ := srcs.At(i).ChunkHash()
			b, _ := ch.Bytes()
			concatenated = append(concatenated, b)
		}
		h := blake3.New()
		for _, b := range concatenated {
			_, _ = h.Write(b)
		}
		expected := h.Sum(nil)
		actualRoot, _ := lf.Root()
		actualBytes, _ := actualRoot.Bytes()
		if !bytesEqual(actualBytes, expected) {
			return fmt.Errorf("root drift: lockfile says %x, BLAKE3(chunkHashes) is %x",
				actualBytes, expected)
		}
	}

	_, _ = fmt.Fprintf(out, "restored %d sources to %s\n", srcs.Len(), outDBPath)
	if verify {
		_, _ = fmt.Fprintln(out, "verify: all chunk hashes + root chain OK")
	}
	return nil
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ─────────────────────────────────────────────────────────────────────
// Phase 3: remote push/pull via build-cache/v1 OCI transport
// ─────────────────────────────────────────────────────────────────────

// runCacheRemotePush walks a local push'd <localDir> and uploads
// everything to the registry per
// cloister-spec/build-cache/v1/wire/push-protocol.md.
func runCacheRemotePush(ctx context.Context, out io.Writer, localDir, baseURL, producer, scope, tag, token string) error {
	configBytes, err := os.ReadFile(filepath.Join(localDir, "mache.lock.bin"))
	if err != nil {
		return fmt.Errorf("read mache.lock.bin: %w", err)
	}
	configDigest := digestFor(blake3.Sum256(configBytes))

	chunksByDigest := map[string][]byte{}
	chunkLayers := []OCIDescriptor{}
	objectsDir := filepath.Join(localDir, "objects")
	bucketEntries, err := os.ReadDir(objectsDir)
	if err != nil {
		return fmt.Errorf("read objects dir: %w", err)
	}
	for _, bucketEntry := range bucketEntries {
		if !bucketEntry.IsDir() {
			continue
		}
		bucketName := bucketEntry.Name()
		if len(bucketName) != 2 {
			continue
		}
		bucketPath := filepath.Join(objectsDir, bucketName)
		files, err := os.ReadDir(bucketPath)
		if err != nil {
			return fmt.Errorf("read bucket %s: %w", bucketPath, err)
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			fileName := f.Name()
			if len(fileName) != 62 {
				continue
			}
			fullHex := bucketName + fileName
			digest := "sha256:" + fullHex
			body, err := os.ReadFile(filepath.Join(bucketPath, fileName))
			if err != nil {
				return fmt.Errorf("read chunk %s: %w", fileName, err)
			}
			actual := blake3.Sum256(body)
			if hex.EncodeToString(actual[:]) != fullHex {
				return fmt.Errorf("chunk %s drift on push", fileName)
			}
			chunksByDigest[digest] = body
			chunkLayers = append(chunkLayers, OCIDescriptor{
				MediaType: cacheLayerMediaType,
				Digest:    digest,
				Size:      int64(len(body)),
			})
		}
	}

	manifest := &OCIManifest{
		SchemaVersion: 2,
		MediaType:     ociManifestMediaType,
		Config: OCIDescriptor{
			MediaType: cacheConfigMediaType,
			Digest:    configDigest,
			Size:      int64(len(configBytes)),
		},
		Layers: chunkLayers,
		Annotations: map[string]string{
			"org.cloister.build-cache.producer":         producer,
			"org.cloister.build-cache.producer_version": MacheProducerVersion,
			"org.cloister.build-cache.schema_version":   CacheVersion,
		},
	}

	client, err := NewOCIClient(baseURL, producer, scope)
	if err != nil {
		return err
	}
	if token != "" {
		client.SetToken(token)
	}

	manifestDigest, err := client.PushBundle(ctx, manifest, configBytes, chunksByDigest, tag, 4)
	if err != nil {
		return fmt.Errorf("remote push: %w", err)
	}

	_, _ = fmt.Fprintf(out, "pushed %d chunks + manifest to %s/v2/%s/%s\n",
		len(chunkLayers), baseURL, producer, scope)
	_, _ = fmt.Fprintf(out, "manifest digest: %s\n", manifestDigest)
	if tag != "" {
		_, _ = fmt.Fprintf(out, "tag: %s\n", tag)
	}
	return nil
}

// runCacheRemotePull fetches a manifest + config + chunks from the
// registry into <localDir>, mirroring what runCachePush emits. After
// this returns, runCachePull(localDir, outDB) can restore as if the
// bundle had originated locally.
func runCacheRemotePull(ctx context.Context, out io.Writer, baseURL, producer, scope, ref, token, localDir string) error {
	client, err := NewOCIClient(baseURL, producer, scope)
	if err != nil {
		return err
	}
	if token != "" {
		client.SetToken(token)
	}

	manifest, configBytes, chunks, manifestDigest, err := client.PullBundle(ctx, ref, 4)
	if err != nil {
		return fmt.Errorf("remote pull: %w", err)
	}

	if err := os.MkdirAll(filepath.Join(localDir, "objects"), 0o755); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(localDir, "mache.lock.bin"), configBytes); err != nil {
		return fmt.Errorf("write lockfile: %w", err)
	}
	for _, layer := range manifest.Layers {
		body, ok := chunks[layer.Digest]
		if !ok {
			return fmt.Errorf("pulled manifest references chunk %s but body missing", layer.Digest)
		}
		hexDigest := layer.Digest
		if len(hexDigest) > len("sha256:") && hexDigest[:len("sha256:")] == "sha256:" {
			hexDigest = hexDigest[len("sha256:"):]
		}
		if len(hexDigest) != 64 {
			return fmt.Errorf("layer digest %q: want 64 hex chars after sha256 prefix", layer.Digest)
		}
		bucket := filepath.Join(localDir, "objects", hexDigest[:2])
		if err := os.MkdirAll(bucket, 0o755); err != nil {
			return fmt.Errorf("mkdir bucket: %w", err)
		}
		chunkPath := filepath.Join(bucket, hexDigest[2:])
		if existing, err := os.ReadFile(chunkPath); err == nil {
			if !bytesEqual(existing, body) {
				return fmt.Errorf("chunk %s on disk has wrong bytes", chunkPath)
			}
			continue
		}
		if err := writeFileAtomic(chunkPath, body); err != nil {
			return fmt.Errorf("write chunk %s: %w", chunkPath, err)
		}
	}

	_, _ = fmt.Fprintf(out, "pulled manifest %s (%d chunks) from %s/v2/%s/%s\n",
		manifestDigest, len(manifest.Layers), baseURL, producer, scope)
	return nil
}
