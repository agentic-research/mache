// Package graph — store.go defines the pluggable storage interface for
// persisting and restoring projected graphs to/from remote backends.
//
// The unit of persistence is a SQLite .db file (produced by SQLiteWriter
// or mache build). This keeps the abstraction simple: backends just need
// to store and retrieve opaque blobs keyed by repo identity + generation.
//
// Implementations: local disk (default), Cloudflare R2, GCS, S3.
package graph

import (
	"context"
	"io"
	"time"
)

// GraphStore persists and retrieves projected graph databases.
// The key is a repo identifier (e.g., "github.com/org/repo") and the
// value is a SQLite .db file containing the nodes/refs/defs tables.
type GraphStore interface {
	// Put persists a graph database for the given repo.
	// The reader provides the SQLite .db bytes. Generation is an
	// opaque version counter (monotonically increasing).
	Put(ctx context.Context, repo string, generation uint64, r io.Reader) error

	// Get retrieves the latest graph database for the given repo.
	// Returns the reader, generation, and modification time.
	// Returns ErrGraphNotCached if no graph exists for this repo.
	Get(ctx context.Context, repo string) (io.ReadCloser, uint64, error)

	// Head returns metadata without fetching the full database.
	// Used for cache validation (is my local copy still current?).
	Head(ctx context.Context, repo string) (*GraphMeta, error)

	// Delete removes a persisted graph for the given repo.
	Delete(ctx context.Context, repo string) error

	// List returns all repos with persisted graphs.
	// Use for garbage collection and admin tooling.
	List(ctx context.Context) ([]GraphMeta, error)
}

// GraphMeta is metadata about a persisted graph.
type GraphMeta struct {
	Repo       string    // e.g., "github.com/org/repo"
	Generation uint64    // monotonic version counter
	Size       int64     // .db file size in bytes
	ModTime    time.Time // last update time
	ETag       string    // content hash for conditional gets (optional)
}

// ErrGraphNotCached is returned by Get/Head when no graph exists for the repo.
type ErrGraphNotCached struct {
	Repo string
}

func (e *ErrGraphNotCached) Error() string {
	return "graph not cached: " + e.Repo
}

// LocalStore persists graphs to a local directory.
// Each repo gets a subdirectory: {base}/{repo-hash}/graph.db
type LocalStore struct {
	BaseDir string
}

// R2Store persists graphs to Cloudflare R2.
// Key format: graphs/{repo-hash}/graph.db
// Debounce writes to avoid thrashing (caller's responsibility).
type R2Store struct {
	Bucket    string
	AccountID string
	AccessKey string
	SecretKey string
}
