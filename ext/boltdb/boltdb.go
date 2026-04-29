// Package boltdb provides a materializer that converts a mache SQLite DB
// into a BoltDB file. Isolated in its own Go module to keep the go.etcd.io/bbolt
// dependency out of the root go.mod (avoids vulnerability scanner noise).
package boltdb

import (
	"database/sql"
	"fmt"
	"os"

	bolt "go.etcd.io/bbolt"
	_ "modernc.org/sqlite"
)

// nodeKindFile and nodeKindDir mirror internal/graph.NodeKindFile/Dir.
// Duplicated here because this is a workspace module that can't import
// the internal package — the values are part of mache's on-disk schema
// and must stay in sync.
const (
	nodeKindFile = 0
	nodeKindDir  = 1
)

// Materialize reads the node tree from a mache SQLite DB and writes
// directories as bbolt nested buckets and file content as bucket key/values.
func Materialize(srcDB, outPath string) error {
	db, err := sql.Open("sqlite", srcDB)
	if err != nil {
		return fmt.Errorf("open source db: %w", err)
	}
	defer func() { _ = db.Close() }()

	if err := os.Remove(outPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove existing output %q: %w", outPath, err)
	}

	bdb, err := bolt.Open(outPath, 0o600, nil)
	if err != nil {
		return fmt.Errorf("create boltdb: %w", err)
	}
	defer func() { _ = bdb.Close() }()

	rows, err := db.Query(`SELECT id, COALESCE(parent_id, ''), name, kind, record FROM nodes ORDER BY id`)
	if err != nil {
		return fmt.Errorf("query nodes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type node struct {
		id, parentID, name string
		kind               int
		content            sql.NullString
	}

	var nodes []node
	childrenOf := map[string][]int{}
	for rows.Next() {
		var n node
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

	var writeChildren func(tx *bolt.Tx, pb *bolt.Bucket, parentID string, visited map[string]bool) error
	writeChildren = func(tx *bolt.Tx, pb *bolt.Bucket, parentID string, visited map[string]bool) error {
		for _, idx := range childrenOf[parentID] {
			n := nodes[idx]
			if visited[n.id] {
				continue
			}
			visited[n.id] = true

			if n.kind == nodeKindDir {
				if n.name == "" && pb == nil {
					if err := writeChildren(tx, pb, n.id, visited); err != nil {
						return err
					}
					continue
				}
				var bucket *bolt.Bucket
				var err error
				if pb == nil {
					bucket, err = tx.CreateBucketIfNotExists([]byte(n.name))
				} else {
					bucket, err = pb.CreateBucketIfNotExists([]byte(n.name))
				}
				if err != nil {
					return fmt.Errorf("create bucket %q: %w", n.name, err)
				}
				if err := writeChildren(tx, bucket, n.id, visited); err != nil {
					return err
				}
			} else if n.kind == nodeKindFile && n.content.Valid && n.name != "" {
				if pb == nil {
					root, err := tx.CreateBucketIfNotExists([]byte("_root"))
					if err != nil {
						return err
					}
					_ = root.Put([]byte(n.name), []byte(n.content.String))
				} else {
					_ = pb.Put([]byte(n.name), []byte(n.content.String))
				}
			}
		}
		return nil
	}

	return bdb.Update(func(tx *bolt.Tx) error {
		return writeChildren(tx, nil, "", map[string]bool{})
	})
}
