package lloschema

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeebo/blake3"
)

// Every read of leyline's stored file bytes (source_blobs.blob_bytes) goes
// through this file, and is verified: the bytes must hash to the key they are
// stored under. leyline's blob_hash is BLAKE3 of the raw bytes, the one
// unkeyed construction under its ADR-0047 (checked against a real parse,
// mache-10a306). A row whose bytes were changed under their hash would
// otherwise be served as that file: into a write's base, read_file, the
// AST walker, the build cache and smell snippets (mache-c1eb7a, mirroring
// ley-line-open-12d305). internal/lint refuses a blob_bytes read anywhere
// else.

var (
	// ErrNoBlob: source_blobs holds no row for the hash.
	ErrNoBlob = errors.New("no stored bytes for this blob hash")
	// ErrBlobChunked: the row exists but its bytes are NULL, held as CDC
	// chunks instead (leyline's ChunkActivated). Never an empty file.
	ErrBlobChunked = errors.New("the blob's bytes are held as chunks, not inline")
	// ErrBlobTampered: the stored bytes do not hash to their key.
	ErrBlobTampered = errors.New("stored bytes do not match their blob hash")
	// ErrNoSourceBytes: a _source row names no blob and carries no inline
	// content.
	ErrNoSourceBytes = errors.New("the projection holds no bytes for this source")
)

// VerifyBlob returns nil when content hashes to hash, and an error naming the
// hash when it does not.
func VerifyBlob(hash, content []byte) error {
	got := blake3.Sum256(content)
	if !bytes.Equal(got[:], hash) {
		return fmt.Errorf("blob %x: %w (the bytes hash to %x)", hash, ErrBlobTampered, got[:])
	}
	return nil
}

// ReadBlob returns the verified bytes source_blobs holds for hash.
func ReadBlob(qg Querier, hash []byte) ([]byte, error) {
	rows, err := qg.QueryRefs(`SELECT blob_bytes, blob_bytes IS NULL FROM source_blobs WHERE blob_hash = ?`, hash)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("blob %x: %w", hash, ErrNoBlob)
	}
	var b sql.RawBytes
	var null bool
	if err := rows.Scan(&b, &null); err != nil {
		return nil, err
	}
	if null {
		return nil, fmt.Errorf("blob %x: %w", hash, ErrBlobChunked)
	}
	content := append([]byte{}, b...)
	if err := VerifyBlob(hash, content); err != nil {
		return nil, err
	}
	return content, nil
}

// SourceColumns is the SELECT list a SourceScan scans, for a query that reads
// `_source` under its own name (no alias): the row's inline content, its blob
// hash, and the blob's bytes and state. Which columns exist depends on the db
// (mache's cache-hydrated db carries inline content and no hash; leyline
// leaves content NULL and keys source_blobs by content_hash), so it is built
// per db.
func SourceColumns(qg Querier) (string, error) {
	hasHash, err := TableHasColumn(qg, "_source", "content_hash")
	if err != nil {
		return "", err
	}
	hasBlobs, err := TableHasColumn(qg, "source_blobs", "blob_bytes")
	if err != nil {
		return "", err
	}
	hash, blob, state := "NULL", "NULL", "NULL"
	if hasHash {
		hash = "_source.content_hash"
	}
	if hasHash && hasBlobs {
		const row = " FROM source_blobs WHERE source_blobs.blob_hash = _source.content_hash)"
		blob = "(SELECT source_blobs.blob_bytes" + row
		// NULL: no row; 1: the row's bytes are NULL (chunked); 0: held.
		state = "(SELECT source_blobs.blob_bytes IS NULL" + row
	}
	return "_source.content, _source.content IS NULL, " + hash + ", " + blob + ", " + state, nil
}

// SourceScan receives one row of SourceColumns. Its fields are unexported so
// the bytes are reachable only through Bytes, which verifies them.
type SourceScan struct {
	inline     []byte
	inlineNull bool
	hash       []byte
	blob       []byte
	state      sql.NullInt64
}

// Targets are the Scan destinations for SourceColumns, in order.
func (s *SourceScan) Targets() []any {
	return []any{&s.inline, &s.inlineNull, &s.hash, &s.blob, &s.state}
}

// Hash is the blob hash the row names, nil when it names none.
func (s *SourceScan) Hash() []byte { return s.hash }

// Bytes returns the row's bytes, verified against its hash when it names one.
// Inline content is used when present; otherwise the blob, which may be
// absent (ErrNoBlob), held as chunks (ErrBlobChunked), or tampered
// (ErrBlobTampered). A row naming no hash can only carry inline content,
// mache's own unhashed copy.
func (s *SourceScan) Bytes() ([]byte, error) {
	if s.hash == nil {
		if s.inlineNull {
			return nil, ErrNoSourceBytes
		}
		return nonNil(s.inline), nil
	}
	content := nonNil(s.inline)
	if s.inlineNull {
		switch {
		case !s.state.Valid:
			return nil, fmt.Errorf("blob %x: %w", s.hash, ErrNoBlob)
		case s.state.Int64 == 1:
			return nil, fmt.Errorf("blob %x: %w", s.hash, ErrBlobChunked)
		}
		content = nonNil(s.blob)
	}
	if err := VerifyBlob(s.hash, content); err != nil {
		return nil, err
	}
	return content, nil
}

// nonNil keeps an empty file empty rather than nil: a zero-length BLOB can
// scan as nil, and nil means "no bytes" to callers.
func nonNil(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}
