//go:build boltdb

package materialize

import (
	"database/sql"
	"fmt"
	"os"

	bolt "go.etcd.io/bbolt"
	_ "modernc.org/sqlite"
)

// BoltDBMaterializer reads the node tree from a mache SQLite DB and writes
// directories as bbolt nested buckets and file content as bucket key/values.
//
// Uses parent_id + name traversal (not path splitting on /) so that node names
// containing slashes, parens, or other special characters work correctly.
// e.g. "CVE-2018-14466 (AFS/RX)" is a valid node name.
type BoltDBMaterializer struct{}

type boltNode struct {
	id       string
	parentID string
	name     string
	kind     int // 1=dir, 0=file
	content  sql.NullString
}

func (m *BoltDBMaterializer) Materialize(srcDB, outPath string) error {
	db, err := sql.Open("sqlite", srcDB)
	if err != nil {
		return fmt.Errorf("open source db: %w", err)
	}
	defer func() { _ = db.Close() }()

	// Remove existing output — bbolt won't overwrite.
	if err := os.Remove(outPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove existing output %q: %w", outPath, err)
	}

	bdb, err := bolt.Open(outPath, 0o600, nil)
	if err != nil {
		return fmt.Errorf("create boltdb: %w", err)
	}
	defer func() { _ = bdb.Close() }()

	// Build the full tree in memory: parent_id → []child rows.
	// This avoids splitting IDs on "/" which breaks on names containing "/".
	rows, err := db.Query(`SELECT id, COALESCE(parent_id, ''), name, kind, record FROM nodes ORDER BY id`)
	if err != nil {
		return fmt.Errorf("query nodes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var nodes []boltNode
	childrenOf := map[string][]int{} // parent_id → indices into nodes
	for rows.Next() {
		var n boltNode
		if err := rows.Scan(&n.id, &n.parentID, &n.name, &n.kind, &n.content); err != nil {
			return fmt.Errorf("scan node: %w", err)
		}
		idx := len(nodes)
		nodes = append(nodes, n)
		childrenOf[n.parentID] = append(childrenOf[n.parentID], idx)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate nodes: %w", err)
	}

	// Recursively create buckets and write files.
	err = bdb.Update(func(tx *bolt.Tx) error {
		return materializeBoltChildren(tx, nil, "", nodes, childrenOf)
	})
	if err != nil {
		return fmt.Errorf("materialize tree: %w", err)
	}

	return nil
}

// materializeBoltChildren recursively creates buckets for directory nodes
// and writes key/value pairs for file nodes under the given parent bucket.
// parentBucket is nil for the root transaction level.
func materializeBoltChildren(tx *bolt.Tx, parentBucket *bolt.Bucket, parentID string, nodes []boltNode, childrenOf map[string][]int) error {
	children, ok := childrenOf[parentID]
	if !ok {
		return nil
	}

	for _, idx := range children {
		n := nodes[idx]

		// Guard against self-referencing nodes (e.g. root with id="" parent_id="").
		if n.id == parentID {
			continue
		}

		if n.kind == 1 {
			// Directory node → create bucket.
			if n.name == "" {
				// Transparent root: don't create a bucket, but process children
				// as if they're at the current level.
				if err := materializeBoltChildren(tx, parentBucket, n.id, nodes, childrenOf); err != nil {
					return err
				}
				continue
			}

			var bucket *bolt.Bucket
			var err error
			if parentBucket == nil {
				bucket, err = tx.CreateBucketIfNotExists([]byte(n.name))
			} else {
				bucket, err = parentBucket.CreateBucketIfNotExists([]byte(n.name))
			}
			if err != nil {
				return fmt.Errorf("create bucket %q: %w", n.name, err)
			}

			// Recurse into children of this directory.
			if err := materializeBoltChildren(tx, bucket, n.id, nodes, childrenOf); err != nil {
				return err
			}

		} else if n.kind == 0 && n.content.Valid {
			// File node → write key/value.
			if n.name == "" {
				continue // skip unnamed files
			}
			if parentBucket == nil {
				// Root-level file → _root bucket.
				root, err := tx.CreateBucketIfNotExists([]byte("_root"))
				if err != nil {
					return fmt.Errorf("create _root bucket: %w", err)
				}
				if err := root.Put([]byte(n.name), []byte(n.content.String)); err != nil {
					return fmt.Errorf("put root file %q: %w", n.name, err)
				}
			} else {
				if err := parentBucket.Put([]byte(n.name), []byte(n.content.String)); err != nil {
					return fmt.Errorf("put file %q: %w", n.name, err)
				}
			}
		}
	}

	return nil
}

func init() {
	registerFormat("boltdb", func() Materializer { return &BoltDBMaterializer{} })
}
