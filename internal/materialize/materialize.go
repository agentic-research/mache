package materialize

import (
	"archive/zip"
	"database/sql"
	"fmt"
	"io"
	"os"
	"strings"

	_ "modernc.org/sqlite"
)

// Materializer writes a projected node tree (from a mache SQLite DB) to an
// output format.
type Materializer interface {
	Materialize(srcDB, outPath string) error
}

// formatRegistry holds optional materializer constructors registered via init().
var formatRegistry = map[string]func() Materializer{}

// registerFormat adds a materializer factory. Used by build-tag gated files
// (e.g., boltdb.go with //go:build boltdb).
func registerFormat(name string, fn func() Materializer) { //nolint:unused // called from build-tag gated init()
	formatRegistry[name] = fn
}

// ForFormat returns the appropriate Materializer for the given format string.
func ForFormat(format string) (Materializer, error) {
	switch format {
	case "sqlite":
		return &SQLiteMaterializer{}, nil
	case "zip":
		return &ZIPMaterializer{}, nil
	}

	// Check optional (build-tag gated) formats.
	if fn, ok := formatRegistry[format]; ok {
		return fn(), nil
	}

	// Build the supported-format list dynamically (core + registered).
	supported := []string{"sqlite", "zip"}
	for name := range formatRegistry {
		supported = append(supported, name)
	}
	return nil, fmt.Errorf("unknown output format: %q (supported: %s)", format, strings.Join(supported, ", "))
}

// ---------------------------------------------------------------------------
// SQLite materializer — file copy
// ---------------------------------------------------------------------------

// SQLiteMaterializer copies the source DB as-is to the output path.
type SQLiteMaterializer struct{}

func (m *SQLiteMaterializer) Materialize(srcDB, outPath string) error {
	in, err := os.Open(srcDB)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy: %w", err)
	}
	return out.Close()
}

// ---------------------------------------------------------------------------
// ZIP materializer — file nodes become archive entries
// ---------------------------------------------------------------------------

// ZIPMaterializer reads the node tree from a mache SQLite DB and writes all
// file nodes (kind=0) with non-NULL content as entries in a ZIP archive.
// Directory nodes are implicit (paths contain slashes).
type ZIPMaterializer struct{}

func (m *ZIPMaterializer) Materialize(srcDB, outPath string) error {
	db, err := sql.Open("sqlite", srcDB)
	if err != nil {
		return fmt.Errorf("open source db: %w", err)
	}
	defer func() { _ = db.Close() }()

	rows, err := db.Query(`SELECT id, record FROM nodes WHERE kind = 0 AND record IS NOT NULL ORDER BY id`)
	if err != nil {
		return fmt.Errorf("query nodes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}

	w := zip.NewWriter(f)

	for rows.Next() {
		var id, content string
		if err := rows.Scan(&id, &content); err != nil {
			_ = w.Close()
			_ = f.Close()
			return fmt.Errorf("scan row: %w", err)
		}

		fw, err := w.Create(id)
		if err != nil {
			_ = w.Close()
			_ = f.Close()
			return fmt.Errorf("create zip entry %s: %w", id, err)
		}
		if _, err := fw.Write([]byte(content)); err != nil {
			_ = w.Close()
			_ = f.Close()
			return fmt.Errorf("write zip entry %s: %w", id, err)
		}
	}
	if err := rows.Err(); err != nil {
		_ = w.Close()
		_ = f.Close()
		return fmt.Errorf("iterate rows: %w", err)
	}

	if err := w.Close(); err != nil {
		_ = f.Close()
		return fmt.Errorf("close zip writer: %w", err)
	}
	return f.Close()
}
