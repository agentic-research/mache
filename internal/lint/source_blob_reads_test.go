package lint

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// sourceBlobBytesAllowlist names the only non-test files whose string literals
// may mention source_blobs.blob_bytes, with why. Every READ goes through
// internal/lloschema/blob.go, which verifies the bytes against their hash
// (mache-c1eb7a); a raw SELECT anywhere else would serve bytes changed under
// their hash as the file. The allowlist may only shrink.
var sourceBlobBytesAllowlist = map[string]string{
	"internal/lloschema/blob.go":              "the verifying reader: ReadBlob, SourceColumns",
	"internal/fixturedb/emit.go":              "fixture WRITER: inserts the blobs it then reads back through lloschema",
	"internal/fixturedb/schema_leyline.go":    "DDL mirror of leyline's table, re-derived from the binary",
	"internal/fixturedb/schema_leyline_v6.go": "DDL mirror of leyline's table, re-derived from the binary",
}

// TestSourceBlobBytes_ReadOnlyThroughTheVerifyingReader fails when production
// code outside the allowlist names blob_bytes in SQL: a new raw read of
// leyline's stored file bytes, unverified. Restoring any of the three raw reads
// mache-c1eb7a removed (ParseBlobs.Blob, TreeFileBytes, SourceBytesExpr)
// turns it red.
func TestSourceBlobBytes_ReadOnlyThroughTheVerifyingReader(t *testing.T) {
	root := repoRootForBoundary(t)
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata", "vendor", ".claude", "bin":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if _, ok := sourceBlobBytesAllowlist[rel]; ok {
			return nil
		}
		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			return nil //nolint:nilerr // unparseable Go is a different test's problem
		}
		// String literals only: SQL lives there, and a comment naming the
		// column is not a read.
		ast.Inspect(file, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && strings.Contains(lit.Value, "blob_bytes") {
				found = append(found, rel)
				return false
			}
			return true
		})
		return nil
	})
	require.NoError(t, err)
	if len(found) > 0 {
		sort.Strings(found)
		t.Errorf("files outside sourceBlobBytesAllowlist name source_blobs.blob_bytes in SQL:\n  %s\n\n"+
			"Read stored file bytes through lloschema.ReadBlob, lloschema.TreeFileBytes or "+
			"lloschema.SourceColumns + SourceScan.Bytes, which verify them against their hash "+
			"(mache-c1eb7a).", strings.Join(found, "\n  "))
	}
}

// TestSourceBlobBytes_AllowlistEntriesStillExist: an entry for a deleted or
// renamed file is a stale exemption that would re-permit a raw read if the
// path came back.
func TestSourceBlobBytes_AllowlistEntriesStillExist(t *testing.T) {
	root := repoRootForBoundary(t)
	for rel := range sourceBlobBytesAllowlist {
		_, err := os.Stat(filepath.Join(root, rel))
		require.NoError(t, err, "sourceBlobBytesAllowlist names %s, which no longer exists — remove the entry", rel)
	}
}
