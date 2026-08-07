package graph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// NewLocalStore creates a LocalStore backed by the given directory.
func NewLocalStore(baseDir string) *LocalStore {
	return &LocalStore{BaseDir: baseDir}
}

func (s *LocalStore) repoDir(repo string) string {
	h := sha256.Sum256([]byte(repo))
	return filepath.Join(s.BaseDir, hex.EncodeToString(h[:8]))
}

func (s *LocalStore) dbPath(repo string) string {
	return filepath.Join(s.repoDir(repo), "graph.db")
}

func (s *LocalStore) metaPath(repo string) string {
	return filepath.Join(s.repoDir(repo), "meta.json")
}

// Put persists a graph database to local disk.
func (s *LocalStore) Put(_ context.Context, repo string, generation uint64, r io.Reader) error {
	dir := s.repoDir(repo)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	// Write to temp file then rename (atomic)
	tmp, err := os.CreateTemp(dir, "graph-*.db.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()

	size, err := io.Copy(tmp, r)
	if err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write: %w", err)
	}
	_ = tmp.Close()

	dbPath := s.dbPath(repo)
	if err := os.Rename(tmpPath, dbPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename: %w", err)
	}

	// Write metadata sidecar
	meta := GraphMeta{
		Repo:       repo,
		Generation: generation,
		Size:       size,
		ModTime:    time.Now().UTC(),
	}
	metaJSON, _ := json.Marshal(meta)
	if err := os.WriteFile(s.metaPath(repo), metaJSON, 0o644); err != nil {
		return fmt.Errorf("write meta: %w", err)
	}

	return nil
}

// Get retrieves the graph database from local disk.
func (s *LocalStore) Get(_ context.Context, repo string) (io.ReadCloser, uint64, error) {
	meta, err := s.Head(context.Background(), repo)
	if err != nil {
		return nil, 0, err
	}

	f, err := os.Open(s.dbPath(repo))
	if err != nil {
		return nil, 0, &ErrGraphNotCached{Repo: repo}
	}

	return f, meta.Generation, nil
}

// Head returns metadata without reading the full database.
func (s *LocalStore) Head(_ context.Context, repo string) (*GraphMeta, error) {
	data, err := os.ReadFile(s.metaPath(repo))
	if err != nil {
		return nil, &ErrGraphNotCached{Repo: repo}
	}

	var meta GraphMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("parse meta: %w", err)
	}

	return &meta, nil
}

// Delete removes a persisted graph from local disk.
func (s *LocalStore) Delete(_ context.Context, repo string) error {
	return os.RemoveAll(s.repoDir(repo))
}

// List returns all repos with persisted graphs.
func (s *LocalStore) List(_ context.Context) ([]GraphMeta, error) {
	entries, err := os.ReadDir(s.BaseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var metas []GraphMeta
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		metaPath := filepath.Join(s.BaseDir, e.Name(), "meta.json")
		data, err := os.ReadFile(metaPath)
		if err != nil {
			continue
		}
		var meta GraphMeta
		if json.Unmarshal(data, &meta) == nil {
			metas = append(metas, meta)
		}
	}

	sort.Slice(metas, func(i, j int) bool {
		return metas[i].ModTime.After(metas[j].ModTime)
	})

	return metas, nil
}
