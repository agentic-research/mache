//go:build leyline

package ingest

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

	"github.com/agentic-research/mache/internal/lang"
)

// parityFixtureFiles is a small Go project with functions, methods, and
// types — enough shape variety to exercise field extraction (name,
// parameters, result, body, receiver, operator, ...). Filenames sort
// lexicographically to match FlattenASTDB's ORDER BY source_id.
var parityFixtureFiles = map[string][]byte{
	"main.go": []byte(`package demo

import "fmt"

const MaxRetries = 3

var DefaultName = "world"

func Hello() string {
	return "hello"
}

func Caller() {
	fmt.Println(Hello())
}
`),
	"types.go": []byte(`package demo

type Greeter struct {
	Name string
}

type Speaker interface {
	Speak() string
}

func (g *Greeter) Greet() string {
	return "Hi, " + g.Name
}

func (g Greeter) String() string {
	return g.Name
}
`),
}

// resolvePinnedLeyline resolves the pinned leyline binary without
// downloading (tests never download), mirroring leyline.ResolveBinary(false):
// PATH and ~/.mache/bin candidates, each accepted ONLY if `--version`
// exactly matches the compile-time pin — a bare unverified PATH leyline is
// never trusted (stale local installs produce different _ast output).
//
// The logic is duplicated here rather than calling ResolveBinary directly
// because importing internal/leyline under the `leyline` build tag compiles
// that package's CGO FFI bindings (client.go needs leyline_fs.h), which
// this tagged test must not require. The pin is read from socket.go's
// leylineBinaryVersion const so a pin bump can't leave this test stale.
func resolvePinnedLeyline(t *testing.T) string {
	t.Helper()
	pin := pinnedLeylineVersion(t)

	var candidates []string
	if p, err := exec.LookPath("leyline"); err == nil {
		candidates = append(candidates, p)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".mache", "bin", "leyline"))
	}
	for _, c := range candidates {
		if leylineVersionEquals(c, pin) {
			return c
		}
	}
	t.Skipf("no leyline matching the pinned %s available (tests never download)", pin)
	return ""
}

// pinnedLeylineVersion extracts the leylineBinaryVersion const from
// internal/leyline/socket.go (the same source-grep pattern
// version_parity_test.go uses for pin invariants).
func pinnedLeylineVersion(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "module root with go.mod not found")
		dir = parent
	}
	src, err := os.ReadFile(filepath.Join(dir, "internal", "leyline", "socket.go"))
	require.NoError(t, err)
	m := regexp.MustCompile(`const leylineBinaryVersion = "(v\d+\.\d+\.\d+)"`).FindSubmatch(src)
	require.NotNil(t, m, "leylineBinaryVersion const not found in internal/leyline/socket.go")
	return string(m[1])
}

// leylineVersionEquals reports whether `<path> --version` reports exactly
// the pinned major.minor.patch (like leylineVersionMatchesPin).
func leylineVersionEquals(path, pin string) bool {
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		return false
	}
	got := regexp.MustCompile(`\d+\.\d+\.\d+`).FindString(string(out))
	return got != "" && "v"+got == pin
}

// TestFlattenASTDBParity_Go verifies that FlattenASTDB over a
// leyline-parsed _ast database produces the same multiset of FCA records as
// FlattenAST over in-process tree-sitter parses of the same sources.
func TestFlattenASTDBParity_Go(t *testing.T) {
	leylineBin := resolvePinnedLeyline(t)

	srcDir := t.TempDir()
	names := make([]string, 0, len(parityFixtureFiles))
	for name, content := range parityFixtureFiles {
		require.NoError(t, os.WriteFile(filepath.Join(srcDir, name), content, 0o644))
		names = append(names, name)
	}
	sort.Strings(names)

	// --- Sitter path (CGO) ---
	goLang := lang.ForName("go")
	require.NotNil(t, goLang)
	var sitterRecords []any
	for _, name := range names {
		parser := sitter.NewParser()
		parser.SetLanguage(goLang.Grammar())
		tree, err := parser.ParseCtx(context.Background(), nil, parityFixtureFiles[name])
		require.NoError(t, err)
		require.NotNil(t, tree)
		sitterRecords = append(sitterRecords, FlattenAST(tree.RootNode())...)
	}

	// --- Leyline path (pure Go) ---
	dbPath := filepath.Join(t.TempDir(), "parity.db")
	out, err := exec.Command(leylineBin, "parse", srcDir, "-o", dbPath).CombinedOutput()
	require.NoError(t, err, "leyline parse failed: %s", string(out))

	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	dbRecords, err := FlattenASTDB(db, "", 0)
	require.NoError(t, err)

	// Grammar-version skew: leyline ships a newer tree-sitter-go than the
	// grammar bundled with smacker/go-tree-sitter. The newer grammar emits
	// two node kinds the old one has no notion of: `statement_list` (blocks
	// used to hold statements directly) and
	// `interpreted_string_literal_content` (string bodies used to be
	// unnamed). Records are per-node independent and neither kind carries
	// fields, so dropping them from BOTH sides leaves every other record
	// untouched — the comparison stays exact for the shared population, and
	// any NEW divergence still fails.
	skewKinds := map[string]bool{
		"statement_list":                     true,
		"interpreted_string_literal_content": true,
	}
	sitterRecords = dropKinds(sitterRecords, skewKinds)
	filtered := dropKinds(dbRecords, skewKinds)

	t.Logf("FlattenAST:   %d records (post skew-filter)", len(sitterRecords))
	t.Logf("FlattenASTDB: %d records (%d raw, post skew-filter)", len(filtered), len(dbRecords))
	require.Equal(t, len(sitterRecords), len(filtered), "record count should match")

	// Multiset comparison via sorted canonical serialization.
	sitterKeys := canonicalRecords(t, sitterRecords)
	dbKeys := canonicalRecords(t, filtered)
	if !assert.Equal(t, sitterKeys, dbKeys, "record multisets should match") {
		logRecordDiffs(t, sitterKeys, dbKeys)
	}
}

func dropKinds(records []any, kinds map[string]bool) []any {
	out := make([]any, 0, len(records))
	for _, rec := range records {
		if m, ok := rec.(map[string]any); ok {
			if typ, _ := m["type"].(string); kinds[typ] {
				continue
			}
		}
		out = append(out, rec)
	}
	return out
}

func canonicalRecords(t *testing.T, records []any) []string {
	t.Helper()
	keys := make([]string, len(records))
	for i, rec := range records {
		// encoding/json sorts map keys — a canonical per-record form.
		data, err := json.Marshal(rec)
		require.NoError(t, err)
		keys[i] = string(data)
	}
	sort.Strings(keys)
	return keys
}

func logRecordDiffs(t *testing.T, sitterKeys, dbKeys []string) {
	t.Helper()
	diffs, si, di := 0, 0, 0
	for si < len(sitterKeys) && di < len(dbKeys) && diffs < 10 {
		switch {
		case sitterKeys[si] == dbKeys[di]:
			si++
			di++
		case sitterKeys[si] < dbKeys[di]:
			t.Logf("  SITTER only: %s", sitterKeys[si])
			si++
			diffs++
		default:
			t.Logf("  ASTDB only:  %s", dbKeys[di])
			di++
			diffs++
		}
	}
}
