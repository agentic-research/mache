package lint

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// regexpAllowlist is the frozen set of files permitted to import "regexp",
// as repo-relative paths. It is a RATCHET, not a blessing: regex is a smell of
// heuristical matching (prefer structural / AST / typed-API matching), so the
// set may only SHRINK. TestRegexpImportRatchet fails when a new file imports
// regexp — forcing a deliberate choice: replace it with a structural match, or
// (rarely) add it here with a one-line justification.
//
// Reducing the entries below — especially the production ones — is tracked
// separately; this test only prevents growth.
var regexpAllowlist = map[string]string{
	// tooling / codegen — regex over generated or external text is acceptable
	"tools/fixtures-rebaseline/main.go": "rebaseline codegen over tool output",
	"tools/fuzz-gen/main.go":            "fuzz corpus generation",

	// parsing external command / file output (no typed API available)
	"internal/lint/tool_preflight.go":         "parses Taskfile text",
	"internal/leyline/validate_op_test.go":    "asserts on leyline op output text",
	"internal/leyline/version_parity_test.go": "parses `--version` output text",
	"internal/buildinfo/buildinfo_test.go":    "parses version string",
	"internal/lattice/date_parsing_test.go":   "date-format heuristics under test",
	"cmd/find_smells_cli_test.go":             "asserts on CLI stdout text",
	"cmd/find_smells_action_test.go":          "asserts on action output text",
	"cmd/call_extractor_test_helper_test.go":  "test fixture text matching",

	// production code — candidates for structural replacement (follow-up)
	"internal/ingest/ast_walker_selector.go": "TODO: prefer structural match",
	"internal/graph/memstore_callees.go":     "TODO: prefer structural match",
	"internal/graph/sqlite_graph_scan.go":    "TODO: prefer structural match",
	"internal/lattice/context.go":            "TODO: prefer structural match",
}

// dirsToSkip are trees that are not mache's own source (vendored fixtures,
// worktrees, build output, VCS).
var dirsToSkip = map[string]bool{
	".git": true, ".claude": true, ".worktrees": true, ".beads": true,
	"testdata": true, "node_modules": true, "dist": true, "dist-verify": true,
	"packages": true, "target": true, "bin": true,
}

// TestRegexpImportRatchet parses every .go file's imports (structurally, via
// go/parser — not by regex-matching source text) and asserts the set of files
// importing "regexp" is a subset of regexpAllowlist. New regexp usage fails
// here. Runs under `task test` → `task ci` → the pre-push hook, so it needs no
// manual invocation and no external lint config.
func TestRegexpImportRatchet(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	var offenders []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if dirsToSkip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return nil // unparseable/generated file — not our concern here
		}
		for _, imp := range f.Imports {
			if imp.Path.Value != `"regexp"` {
				continue
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if _, ok := regexpAllowlist[rel]; !ok {
				offenders = append(offenders, rel)
			}
		}
		return nil
	})
	require.NoError(t, err)

	sort.Strings(offenders)
	require.Empty(t, offenders,
		"these files import \"regexp\" but are not in regexpAllowlist.\n"+
			"regex is a smell of heuristical matching — prefer a structural/AST/typed-API\n"+
			"match. If regex is genuinely the right tool, add the file to regexpAllowlist\n"+
			"in internal/lint/regexp_ratchet_test.go with a one-line justification:\n  %s",
		strings.Join(offenders, "\n  "))
}

// TestRegexpAllowlistHasNoStaleEntries keeps the allowlist honest: an entry
// that no longer imports regexp (e.g. after a structural rewrite) must be
// removed, so the ratchet actually tightens over time.
func TestRegexpAllowlistHasNoStaleEntries(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	var stale []string
	for rel := range regexpAllowlist {
		f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, parser.ImportsOnly)
		if err != nil {
			stale = append(stale, rel+" (missing/unparseable)")
			continue
		}
		imports := false
		for _, imp := range f.Imports {
			if imp.Path.Value == `"regexp"` {
				imports = true
				break
			}
		}
		if !imports {
			stale = append(stale, rel+" (no longer imports regexp)")
		}
	}
	sort.Strings(stale)
	require.Empty(t, stale,
		"regexpAllowlist has stale entries — remove them so the ratchet tightens:\n  %s",
		strings.Join(stale, "\n  "))
}

// moduleRoot returns the module root, discovered from this test file's own
// compile-time location (internal/lint/ → ../.. → root), independent of CWD.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller failed")
	root := filepath.Clean(filepath.Join(filepath.Dir(self), "..", ".."))
	require.FileExists(t, filepath.Join(root, "go.mod"), "repo root must contain go.mod")
	return root
}
