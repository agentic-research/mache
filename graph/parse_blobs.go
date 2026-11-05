package graph

import (
	"database/sql"
	"fmt"
	"sync"

	"github.com/agentic-research/mache/internal/lloschema"
)

// ParseBlobs is a BlobSource over a leyline parse db: the parsed bytes
// in source_blobs, keyed by _source.content_hash, plus the bytes of each write
// mache proposed and the daemon accepted. The parse db never holds those, and
// the next write to the same file builds on them (mache-3a0fb9).
type ParseBlobs struct {
	db *sql.DB
	mu sync.Mutex
	// own is each written file's latest accepted bytes, by file path; only
	// the latest can be a write's base.
	own map[string]ownBlob
}

type ownBlob struct {
	hash    string
	content []byte
}

// NewParseBlobs returns the BlobSource for the parse db db.
func NewParseBlobs(db *sql.DB) *ParseBlobs {
	return &ParseBlobs{db: db, own: map[string]ownBlob{}}
}

// Blob implements BlobSource. A remembered write's bytes are mache's own, held
// in memory under the hash the daemon returned; the parse db's are read
// through lloschema.ReadBlob, which verifies them (mache-c1eb7a).
func (p *ParseBlobs) Blob(hash []byte) ([]byte, error) {
	if len(hash) == 0 {
		return nil, fmt.Errorf("no blob hash: %w", lloschema.ErrNoBlob)
	}
	p.mu.Lock()
	for _, o := range p.own {
		if o.hash == string(hash) {
			p.mu.Unlock()
			return o.content, nil
		}
	}
	p.mu.Unlock()
	return lloschema.ReadBlob(lloschema.DB(p.db), hash)
}

// Remember implements BlobSource.
func (p *ParseBlobs) Remember(filePath string, hash, content []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.own[filePath] = ownBlob{hash: string(hash), content: content}
}
