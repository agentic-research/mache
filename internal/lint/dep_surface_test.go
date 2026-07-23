package lint

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// thinSurfaceThreshold is the number of distinct exported symbols at or below
// which a dependency is considered "minimal surface area" — a candidate for
// inlining or removal, because we're taking on a whole module's supply-chain
// and build cost for a handful of call sites.
const thinSurfaceThreshold = 2

// thinDepAllowlist records every external dependency whose usage surface is at
// or below thinSurfaceThreshold, with why it earns its keep. It is a RATCHET,
// not a blessing: a NEW thin dependency fails the test, forcing a deliberate
// choice — inline it, or add it here with a justification.
//
// IMPORTANT: thin does NOT mean removable. The x/text entries below are the
// worked example — one call site, but replacing it means hand-rolling Unicode
// title-casing, i.e. heuristic character matching, which is exactly the class
// of thing this repo removes on sight. Cost of the dep vs fidelity of the
// result is a judgement call, so this list carries reasons rather than
// auto-failing.
var thinDepAllowlist = map[string]string{
	// Load-bearing single entry points — the whole point of the dep is one call.
	"github.com/willscott/go-nfs":                 "nfs.Serve — the NFS mount backend (ADR-0006); one entry point by design",
	"github.com/willscott/go-nfs/helpers":         "companion helpers for the go-nfs server above",
	"github.com/go-git/go-billy/v5/helper/chroot": "chroot.New — confines the billy FS the NFS server exposes",
	"github.com/ohler55/ojg/jp":                   "jp.ParseString — the JSONPath engine behind the JSON walker",
	"mvdan.cc/gofumpt/format":                     "format.Source/Options — gofumpt IS the Go formatter for write-back",
	"github.com/zeebo/blake3":                     "blake3.Sum256/New — content addressing; must match ley-line's hash exactly",

	// HCL: three packages, one job (Terraform/HCL write-back validate + format).
	"github.com/hashicorp/hcl/v2":           "hcl.InitialPos/DiagError — diagnostics for HCL write-back validation",
	"github.com/hashicorp/hcl/v2/hclsyntax": "hclsyntax.ParseConfig — HCL syntax validation before splice",
	"github.com/hashicorp/hcl/v2/hclwrite":  "hclwrite.Format — the HCL formatter for write-back",

	// Config/manifest parsing where a hand-rolled parser would be the smell.
	"go.yaml.in/yaml/v3":                      "yaml.Unmarshal — parses Taskfile for the gate preflight",
	"github.com/go-task/task/v3/taskfile/ast": "ast.Taskfile/Task — typed Taskfile model; beats regexing YAML",

	// Cross-repo contract: the shape is defined by ley-line-open, not by us.
	"github.com/agentic-research/ley-line-open/clients/go/leyline-schema/cache": "cache.Get/Load/Delete — the LLO cache wire contract",

	// Test-only.
	"github.com/mark3labs/mcp-go/mcptest": "mcptest.NewUnstartedServer/Server — MCP harness, test-only",

	// THE WORKED EXAMPLE — thin, but removal would be a fidelity regression.
	// Two whole modules for one line (internal/template/render.go):
	//     "title": cases.Title(language.Und).String
	// This is the Unicode-correct title-caser (the strings.Title replacement).
	// Hand-rolling it means heuristic character matching — the exact class of
	// thing this repo removes on sight (feedback_regex_is_a_smell). Keeping a
	// correct implementation beats owning a subtly wrong one.
	"golang.org/x/text/cases":    "cases.Title — Unicode-correct title casing; hand-rolling would be a fidelity regression",
	"golang.org/x/text/language": "language.Und — the locale tag cases.Title requires",
}

// blankImportOnly are dependencies imported solely for their side effects
// (`_ "..."`). They register zero symbols by design — database drivers and the
// like — so surface analysis says nothing about them.
var blankImportOnly = map[string]bool{
	"modernc.org/sqlite": true, // pure-Go SQL driver; registers via init()
}

// depSurface is one dependency's measured usage across the module.
type depSurface struct {
	symbols   map[string]bool // distinct "alias.Symbol" references
	files     map[string]bool // files importing it
	blankOnly bool            // every import of it is `_`
}

// TestDependencySurface measures how much of each external dependency mache
// actually uses, and ratchets against new minimal-surface dependencies.
//
// Run `go test -run TestDependencySurface -v ./internal/lint/` (or
// `task deps:surface`) to see the full report. Package names come from
// `go list` — NOT guessed from the import path, which is wrong for versioned
// modules (github.com/hashicorp/hcl/v2 is package `hcl`, not `v2`).
func TestDependencySurface(t *testing.T) {
	root := moduleRoot(t)
	surfaces := measureDepSurfaces(t, root)
	require.NotEmpty(t, surfaces, "expected to measure at least one dependency")

	// Report, thinnest first — this is the inline/remove candidate list.
	type depRow struct {
		dep   string
		s     *depSurface
		nsyms int
	}
	rows := make([]depRow, 0, len(surfaces))
	for dep, s := range surfaces {
		rows = append(rows, depRow{dep, s, len(s.symbols)})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].nsyms != rows[j].nsyms {
			return rows[i].nsyms < rows[j].nsyms
		}
		return rows[i].dep < rows[j].dep
	})
	t.Logf("dependency usage surface (%d external deps), thinnest first:", len(rows))
	for _, r := range rows {
		kind := ""
		if r.s.blankOnly {
			kind = "  [blank-import only]"
		}
		t.Logf("  %-62s symbols=%-3d files=%-3d%s", r.dep, r.nsyms, len(r.s.files), kind)
	}

	var unjustified []string
	for _, r := range rows {
		if r.s.blankOnly {
			if !blankImportOnly[r.dep] {
				unjustified = append(unjustified,
					r.dep+" (blank-import only — add to blankImportOnly)")
			}
			continue
		}
		if r.nsyms <= thinSurfaceThreshold {
			if _, ok := thinDepAllowlist[r.dep]; !ok {
				unjustified = append(unjustified,
					r.dep+" (symbols="+strconv.Itoa(r.nsyms)+")")
			}
		}
	}
	sort.Strings(unjustified)
	require.Empty(t, unjustified,
		"these dependencies have minimal surface area but no recorded justification.\n"+
			"Taking a whole module for a couple of call sites is a supply-chain and\n"+
			"build cost — inline it, or add it to thinDepAllowlist in\n"+
			"internal/lint/dep_surface_test.go with a one-line reason:\n  %s",
		strings.Join(unjustified, "\n  "))
}

// TestDepSurfaceAllowlistHasNoStaleEntries keeps the allowlist honest: an entry
// that has grown past the threshold, or is no longer used at all, must be
// removed so the list keeps describing reality.
func TestDepSurfaceAllowlistHasNoStaleEntries(t *testing.T) {
	root := moduleRoot(t)
	surfaces := measureDepSurfaces(t, root)

	var stale []string
	for dep := range thinDepAllowlist {
		s, ok := surfaces[dep]
		if !ok {
			stale = append(stale, dep+" (no longer imported)")
			continue
		}
		if len(s.symbols) > thinSurfaceThreshold {
			stale = append(stale, dep+" (surface grew past the threshold — no longer thin)")
		}
	}
	for dep := range blankImportOnly {
		s, ok := surfaces[dep]
		if !ok {
			stale = append(stale, dep+" (no longer imported)")
			continue
		}
		if !s.blankOnly {
			stale = append(stale, dep+" (now used for symbols, not just side effects)")
		}
	}
	sort.Strings(stale)
	require.Empty(t, stale,
		"dependency allowlists have stale entries — remove them so the lists keep\n"+
			"describing reality:\n  %s", strings.Join(stale, "\n  "))
}

// measureDepSurfaces parses every .go file in the module and counts distinct
// symbol references per external dependency.
// parsedGoFile is one module source file plus its repo-relative path.
type parsedGoFile struct {
	rel  string
	file *ast.File
}

// parseModuleGoFiles parses every .go file under root and reports the set of
// external import paths seen, so package names can be resolved in one batch.
func parseModuleGoFiles(t *testing.T, root string) ([]parsedGoFile, map[string]bool) {
	t.Helper()

	var files []parsedGoFile
	extPaths := map[string]bool{}

	fset := token.NewFileSet()
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
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		files = append(files, parsedGoFile{filepath.ToSlash(rel), f})
		for _, im := range f.Imports {
			if p, ok := externalImportPath(im); ok {
				extPaths[p] = true
			}
		}
		return nil
	})
	require.NoError(t, err)
	return files, extPaths
}

// measureDepSurfaces counts distinct symbol references per external dependency.
func measureDepSurfaces(t *testing.T, root string) map[string]*depSurface {
	t.Helper()
	files, extPaths := parseModuleGoFiles(t, root)
	names := resolvePackageNames(t, root, extPaths)

	surfaces := map[string]*depSurface{}
	for _, pf := range files {
		alias := map[string]string{} // local ident -> import path
		for _, im := range pf.file.Imports {
			p, ok := externalImportPath(im)
			if !ok {
				continue
			}
			s := surfaces[p]
			if s == nil {
				s = &depSurface{symbols: map[string]bool{}, files: map[string]bool{}, blankOnly: true}
				surfaces[p] = s
			}
			s.files[pf.rel] = true

			local := names[p] // authoritative package name from `go list`
			if im.Name != nil {
				local = im.Name.Name
			}
			if local == "_" || local == "." {
				continue // side-effect / dot import contributes no measurable surface
			}
			s.blankOnly = false
			if local != "" {
				alias[local] = p
			}
		}
		if len(alias) == 0 {
			continue
		}
		ast.Inspect(pf.file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if p, ok := alias[id.Name]; ok {
				surfaces[p].symbols[id.Name+"."+sel.Sel.Name] = true
			}
			return true
		})
	}
	return surfaces
}

// externalImportPath reports the import path when it is a third-party module
// (not stdlib, not mache itself). Stdlib paths have no dot in their first
// segment.
func externalImportPath(im *ast.ImportSpec) (string, bool) {
	p := strings.Trim(im.Path.Value, `"`)
	if strings.HasPrefix(p, machModulePath) {
		return "", false
	}
	first, _, _ := strings.Cut(p, "/")
	if !strings.Contains(first, ".") {
		return "", false // stdlib
	}
	return p, true
}

const machModulePath = "github.com/agentic-research/mache"

// resolvePackageNames asks the toolchain for each dependency's real package
// name. Deriving it from the import path is wrong for versioned modules
// (.../hcl/v2 is package `hcl`), which silently produces false "unused"
// readings.
func resolvePackageNames(t *testing.T, root string, paths map[string]bool) map[string]string {
	t.Helper()
	if len(paths) == 0 {
		return nil
	}
	args := make([]string, 0, len(paths)+3)
	args = append(args, "list", "-e", "-json")
	for p := range paths {
		args = append(args, p)
	}
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	out, err := cmd.Output()
	require.NoError(t, err, "go list failed — the toolchain is the source of truth for package names")

	names := map[string]string{}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for dec.More() {
		var pkg struct {
			ImportPath string
			Name       string
		}
		if err := dec.Decode(&pkg); err != nil {
			break
		}
		if pkg.Name != "" {
			names[pkg.ImportPath] = pkg.Name
		}
	}
	return names
}
