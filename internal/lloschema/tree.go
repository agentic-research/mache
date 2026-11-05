package lloschema

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// TreeEntry is one entry of a projection-v7 `_tree`: every file and directory
// leyline saw, parsed or not (mache-3a375a). Parsed reports whether a parser
// projected it (a `_source` row); Diagnostic is the parse's message for it
// from `_parse_diagnostics`, "" when there is none.
type TreeEntry struct {
	Path       string
	Name       string
	IsDir      bool
	Size       int64
	Parsed     bool
	Diagnostic string
}

// treeModeDir is `_tree.mode` for a directory (S_IFDIR).
const treeModeDir = 16384

// HasTree reports whether qg is a projection that records its whole tree
// (`_tree`, projection-v7 on).
func HasTree(qg Querier) (bool, error) {
	return TableHasColumn(qg, "_tree", "path")
}

// TreeChildren is dir's direct children in `_tree` ("" is the root), in path
// order. `_tree` is the authority on what exists: a file no parser projected
// is in it and nowhere else. The range is on the primary key, not LIKE, which
// is case-insensitive and reads _ and % in a name as wildcards.
func TreeChildren(qg Querier, dir string) ([]TreeEntry, error) {
	const cols = `SELECT t.path, t.mode, COALESCE(t.size, 0),
			EXISTS(SELECT 1 FROM _source s WHERE s.id = t.path),
			COALESCE((SELECT d.message FROM _parse_diagnostics d WHERE d.path = t.path), '')
		FROM _tree t `
	var rows *sql.Rows
	var err error
	if dir == "" {
		rows, err = qg.QueryRefs(cols + `WHERE t.path != '' AND instr(t.path, '/') = 0 ORDER BY t.path`)
	} else {
		// Children of "pkg" sort in ["pkg/", "pkg0"): '0' follows '/'.
		prefix := dir + "/"
		rows, err = qg.QueryRefs(cols+`WHERE t.path >= ?1 AND t.path < ?2
			AND instr(substr(t.path, length(?1) + 1), '/') = 0 ORDER BY t.path`,
			prefix, dir+"0")
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []TreeEntry
	for rows.Next() {
		var e TreeEntry
		var mode int64
		if err := rows.Scan(&e.Path, &mode, &e.Size, &e.Parsed, &e.Diagnostic); err != nil {
			return nil, err
		}
		e.Name = e.Path[strings.LastIndex(e.Path, "/")+1:]
		e.IsDir = mode == treeModeDir
		out = append(out, e)
	}
	return out, rows.Err()
}

// TreeFileBytes is the stored bytes of the file at path in `_tree`, verified
// against their hash (ReadBlob): leyline keeps every file's bytes in
// source_blobs, parsed or not. ok is false when path is not a file in `_tree`
// or its bytes are not held; bytes held as chunks or tampered are an error.
func TreeFileBytes(qg Querier, path string) (content []byte, ok bool, err error) {
	rows, err := qg.QueryRefs(`SELECT blob_hash FROM _tree WHERE path = ? AND mode != ?`, path, treeModeDir)
	if err != nil {
		return nil, false, err
	}
	var hash []byte
	found := rows.Next()
	if found {
		err = rows.Scan(&hash)
	}
	if cerr := rows.Close(); err == nil {
		err = cerr
	}
	if err != nil || !found {
		return nil, false, err
	}
	content, err = ReadBlob(qg, hash)
	if errors.Is(err, ErrNoBlob) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", path, err)
	}
	return content, true, nil
}
