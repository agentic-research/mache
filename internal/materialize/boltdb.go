//go:build boltdb

package materialize

import (
	"database/sql"
	"fmt"
	"os"
	"strings"

	bolt "go.etcd.io/bbolt"
	_ "modernc.org/sqlite"
)

// BoltDBMaterializer reads the node tree from a mache SQLite DB and writes
// directories as bbolt nested buckets and file content as bucket key/values.
// Tree path = bucket path. Root-level files (parent_id="") go into a "_root"
// bucket.
type BoltDBMaterializer struct{}

func (m *BoltDBMaterializer) Materialize(srcDB, outPath string) error {
	db, err := sql.Open("sqlite", srcDB)
	if err != nil {
		return fmt.Errorf("open source db: %w", err)
	}
	defer func() { _ = db.Close() }()

	// Remove existing output — bbolt won't overwrite.
	_ = os.Remove(outPath)

	bdb, err := bolt.Open(outPath, 0o644, nil)
	if err != nil {
		return fmt.Errorf("create boltdb: %w", err)
	}
	defer func() { _ = bdb.Close() }()

	// First pass: create all directory buckets (kind=1).
	dirRows, err := db.Query(`SELECT id FROM nodes WHERE kind = 1 ORDER BY id`)
	if err != nil {
		return fmt.Errorf("query dirs: %w", err)
	}
	defer func() { _ = dirRows.Close() }()

	var dirs []string
	for dirRows.Next() {
		var id string
		if err := dirRows.Scan(&id); err != nil {
			return fmt.Errorf("scan dir: %w", err)
		}
		dirs = append(dirs, id)
	}
	if err := dirRows.Err(); err != nil {
		return fmt.Errorf("iterate dirs: %w", err)
	}

	err = bdb.Update(func(tx *bolt.Tx) error {
		// Create directory buckets. Order by id ensures parents before children.
		for _, id := range dirs {
			parts := strings.Split(id, "/")
			var b *bolt.Bucket
			for _, part := range parts {
				if b == nil {
					var err error
					b, err = tx.CreateBucketIfNotExists([]byte(part))
					if err != nil {
						return fmt.Errorf("create top bucket %s: %w", part, err)
					}
				} else {
					var err error
					b, err = b.CreateBucketIfNotExists([]byte(part))
					if err != nil {
						return fmt.Errorf("create nested bucket %s in %s: %w", part, id, err)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("create buckets: %w", err)
	}

	// Second pass: write file content (kind=0).
	fileRows, err := db.Query(`SELECT id, parent_id, name, record FROM nodes WHERE kind = 0 AND record IS NOT NULL ORDER BY id`)
	if err != nil {
		return fmt.Errorf("query files: %w", err)
	}
	defer func() { _ = fileRows.Close() }()

	err = bdb.Update(func(tx *bolt.Tx) error {
		for fileRows.Next() {
			var id, parentID, name, content string
			if err := fileRows.Scan(&id, &parentID, &name, &content); err != nil {
				return fmt.Errorf("scan file: %w", err)
			}

			if parentID == "" {
				// Root-level file → _root bucket.
				b, err := tx.CreateBucketIfNotExists([]byte("_root"))
				if err != nil {
					return fmt.Errorf("create _root bucket: %w", err)
				}
				if err := b.Put([]byte(name), []byte(content)); err != nil {
					return fmt.Errorf("put root file %s: %w", name, err)
				}
				continue
			}

			// Navigate to parent bucket.
			parts := strings.Split(parentID, "/")
			var b *bolt.Bucket
			for _, part := range parts {
				if b == nil {
					b = tx.Bucket([]byte(part))
				} else {
					b = b.Bucket([]byte(part))
				}
				if b == nil {
					return fmt.Errorf("parent bucket not found for %s", id)
				}
			}
			if err := b.Put([]byte(name), []byte(content)); err != nil {
				return fmt.Errorf("put file %s: %w", id, err)
			}
		}
		return fileRows.Err()
	})
	if err != nil {
		return fmt.Errorf("write files: %w", err)
	}

	return nil
}

func init() {
	registerFormat("boltdb", func() Materializer { return &BoltDBMaterializer{} })
}
