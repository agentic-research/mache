package lint

// The LLO PHYSICAL-SCHEMA boundary (mache-be17ce).
//
// llo_boundary_test.go in this package enforces the WRITE half of the split —
// mache must not write ley-line-open's tables. This is the READ half: mache
// must not name LLO's physical columns either, because a producer release
// rewrites every site that does.
//
// It is not hypothetical. LLO v0.20.0's projection-v6 renamed `_ast.node_id`
// to `nid`, replaced `_ast.node_kind` with a `kind_id` into `kinds`, dropped
// `source_id` in favour of `nid >> 24`, and replaced `nodes.id`/`name`/
// `parent_id` with `nid`/`name_id`/`parent_nid`. Against the v0.20.0 binary
// that broke 13 packages and ~136 tests, and all fourteen smell rules, from
// four column renames.
//
// The fix was not a shim. Rendering the old path-string id back out of v6
// would reinstate the ~1 GB of path text across six b-trees that v6 exists to
// delete (mache-3688da). Instead mache's own v_ast / v_nodes / v_defs / v_refs
// carry a STABLE VOCABULARY over whatever the producer wrote, and this test is
// what stops the next rule from reaching past them.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/testutil"
)

// lloPhysicalTables are ley-line-open's own tables. A rule naming one in FROM
// or JOIN has reached past the boundary.
var lloPhysicalTables = []string{"_ast", "node_defs", "node_refs", "nodes"}

// tablesRead returns the identifier following each FROM or JOIN in a query.
//
// A token scan, not a regexp: regexp is a smell in this repo and carries its
// own import ratchet (regexp_ratchet_test.go). Splitting on SQL's own
// delimiters is also more honest about what it does — `FROM(x` and `FROM\nx`
// are the same to SQL and to this, where a `\s+` pattern silently misses the
// first.
func tablesRead(query string) []string {
	fields := strings.FieldsFunc(query, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == ',' ||
			r == '(' || r == ')' || r == ';'
	})
	var out []string
	for i, f := range fields {
		kw := strings.ToUpper(f)
		if kw != "FROM" && kw != "JOIN" {
			continue
		}
		if i+1 < len(fields) {
			out = append(out, strings.ToLower(fields[i+1]))
		}
	}
	return out
}

// TestLLOSchemaBoundary_RulesUseOnlyViews is a HARD gate, not a ratchet: the
// rule set reached zero violations in mache-be17ce, and a ratchet seeded at
// zero is just a gate with extra words.
func TestLLOSchemaBoundary_RulesUseOnlyViews(t *testing.T) {
	physical := make(map[string]bool, len(lloPhysicalTables))
	for _, name := range lloPhysicalTables {
		physical[name] = true
	}

	// examples/smell-rules is the copyable starter kit. It matters at least as
	// much as the built-ins: a physical table name that drifts back in there is
	// one users copy into their own repos.
	root := testutil.MacheRepoRoot(t)
	dirs := []string{
		filepath.Join(root, "internal", "smells", "rules"),
		filepath.Join(root, "examples", "smell-rules"),
	}

	checked := 0
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		require.NotEmpty(t, entries, "no rules in %s — the gate would pass vacuously", dir)

		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			name := filepath.Join(filepath.Base(dir), e.Name())
			t.Run(name, func(t *testing.T) {
				body, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
				require.NoError(t, rerr)

				var rule struct {
					Query    string   `json:"Query"`
					Requires []string `json:"Requires"`
				}
				require.NoError(t, json.Unmarshal(body, &rule))

				var offenders []string
				for _, name := range tablesRead(rule.Query) {
					if physical[name] {
						offenders = append(offenders, "query reads "+name)
					}
				}
				for _, req := range rule.Requires {
					if physical[req] {
						offenders = append(offenders, "Requires names "+req)
					}
				}
				sort.Strings(offenders)
				assert.Empty(t, uniq(offenders),
					"this rule reaches past the canonical views to ley-line-open's physical schema, "+
						"so an LLO release renaming a column silently rewrites it. Read v_ast / v_nodes / "+
						"v_defs / v_refs instead, and add the column there if it is missing.")
			})
			checked++
		}
	}
	require.NotZero(t, checked, "no .json rules were examined")
}

// TestLLOSchemaBoundary_RequiresAreResolvable catches the failure that would
// otherwise be SILENT: a rule whose Requires names something missing is
// skipped, not failed, so a typo turns the rule off rather than red. Every
// name must be a real table or one of the views the boundary installs.
func TestLLOSchemaBoundary_RequiresAreResolvable(t *testing.T) {
	installed := map[string]bool{
		"v_ast": true, "v_nodes": true, "v_defs": true, "v_refs": true,
		"v_test_nodes": true, "v_vendored_files": true, "v_doc_refs": true,
	}
	// Tables a backend may legitimately be required to have, which the views
	// do not stand in for.
	physicalOK := map[string]bool{"_lsp": true, "_lsp_defs": true, "_lsp_refs": true, "_lsp_hover": true, "_source": true}

	dir := filepath.Join(testutil.MacheRepoRoot(t), "internal", "smells", "rules")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	seen := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		body, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		require.NoError(t, rerr)
		var rule struct {
			Requires []string `json:"Requires"`
		}
		require.NoError(t, json.Unmarshal(body, &rule))
		for _, req := range rule.Requires {
			seen++
			assert.True(t, installed[req] || physicalOK[req],
				"%s requires %q, which is neither a view the boundary installs nor an "+
					"allowed producer table — an unresolvable Requires SKIPS the rule silently",
				e.Name(), req)
		}
	}
	require.NotZero(t, seen, "no Requires were examined — the gate would pass vacuously")
}

func uniq(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}
