package smells

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/fixturedb"
)

// Every built-in rule reads node ids through the vocabulary, where they are
// opaque: a path on projection-v4, an integer nid on projection-v6. This
// builds one corpus as both and requires every rule to report the same
// findings from each (mache-2dc105). A rule that read meaning into an id's
// shape, as eight did, fails here rather than answering wrongly on v6.

// parityCorpus is a small Go tree with something for each rule to find: an
// unreferenced exported function, a function with branches and a magic
// number, two structurally identical functions, a token defined in two files,
// and a test file that sleeps.
func parityCorpus(t *testing.T, p fixturedb.Producer) *fixturedb.FixtureDB {
	b := fixturedb.New(t, p)
	b.Source("pkg/a.go", "go", "")
	b.Source("pkg/b.go", "go", "")
	b.Source("pkg/a_test.go", "go", "")
	b.ASTNode("pkg/a.go", "source_file", "pkg/a.go", fixturedb.Span{EndByte: 2000, EndRow: 200})
	b.ASTNode("pkg/b.go", "source_file", "pkg/b.go", fixturedb.Span{EndByte: 300, EndRow: 30})
	b.ASTNode("pkg/a_test.go", "source_file", "pkg/a_test.go", fixturedb.Span{EndByte: 300, EndRow: 30})

	// Two functions with the same shape, so duplicate_code groups them, each
	// with a branch (cyclomatic) and a comparison against a literal (magic
	// int). The second starts further down, so each has its own span.
	for i, base := range []int{100, 600} {
		fn := fmt.Sprintf("pkg/a.go/function_declaration_%d", i)
		row := base / 10
		b.ASTNode(fn, "function_declaration", "pkg/a.go",
			fixturedb.Span{StartByte: base, EndByte: base + 400, StartRow: row, EndRow: row + 40})
		b.ASTNode(fn+"/identifier", "identifier", "pkg/a.go", fixturedb.Bytes(base+5, base+10),
			fixturedb.Detail{Field: "name"})
		b.ASTNode(fn+"/block", "block", "pkg/a.go", fixturedb.Bytes(base+20, base+400),
			fixturedb.Detail{Field: "body"})
		b.ASTNode(fn+"/block/if_statement", "if_statement", "pkg/a.go", fixturedb.Bytes(base+30, base+200))
		b.ASTNode(fn+"/block/if_statement/binary_expression", "binary_expression", "pkg/a.go",
			fixturedb.Bytes(base+33, base+40))
		b.ASTNode(fn+"/block/if_statement/binary_expression/identifier", "identifier", "pkg/a.go",
			fixturedb.Bytes(base+33, base+34))
		b.ASTNode(fn+"/block/if_statement/binary_expression/int_literal", "int_literal", "pkg/a.go",
			fixturedb.Bytes(base+38, base+40), fixturedb.Detail{Token: "42"})
		b.ASTNode(fn+"/block/if_statement/block", "block", "pkg/a.go", fixturedb.Bytes(base+41, base+200))
	}
	b.Def("Shared", "pkg/a.go/function_declaration_0", fixturedb.Function)
	b.Def("Unused", "pkg/a.go/function_declaration_1", fixturedb.Function)

	// Shared is defined again in another file: a duplicate definition.
	b.ASTNode("pkg/b.go/function_declaration", "function_declaration", "pkg/b.go",
		fixturedb.Span{StartByte: 10, EndByte: 200, StartRow: 1, EndRow: 20})
	b.Def("Shared", "pkg/b.go/function_declaration", fixturedb.Function)

	// A test that calls Shared and sleeps. The call sites are declared, as a
	// parse writes them: rules join a reference to its site's node.
	const test = "pkg/a_test.go/function_declaration"
	b.ASTNode(test, "function_declaration", "pkg/a_test.go",
		fixturedb.Span{StartByte: 10, EndByte: 200, StartRow: 1, EndRow: 20})
	b.ASTNode(test+"/block", "block", "pkg/a_test.go", fixturedb.Bytes(30, 200))
	b.ASTNode(test+"/block/call_expression_0", "call_expression", "pkg/a_test.go", fixturedb.Bytes(40, 50))
	b.ASTNode(test+"/block/call_expression_1", "call_expression", "pkg/a_test.go", fixturedb.Bytes(60, 80))
	b.Def("TestShared", test, fixturedb.Function)
	b.Ref("Shared", test, test+"/block/call_expression_0", "")
	b.Ref("Sleep", test, test+"/block/call_expression_1", "time")

	// A file defining ten things: god_file reports a file from ten.
	b.Source("pkg/c.go", "go", "")
	b.ASTNode("pkg/c.go", "source_file", "pkg/c.go", fixturedb.Span{EndByte: 1100, EndRow: 110})
	for i := range 10 {
		fn := fmt.Sprintf("pkg/c.go/function_declaration_%d", i)
		b.ASTNode(fn, "function_declaration", "pkg/c.go",
			fixturedb.Span{StartByte: 100 * i, EndByte: 100*i + 90, StartRow: 10 * i, EndRow: 10*i + 9})
		b.Def(fmt.Sprintf("F%d", i), fixturedb.ConstructID(fn), fixturedb.Function)
	}
	_, f := b.Build()
	return f
}

// parityExempt are the rules this corpus cannot exercise, with why. Each is
// still run and compared; it is only excused from being non-empty.
var parityExempt = map[string]string{
	"drift_doc_broken_internal_link":  "reads markdown, which the corpus has none of",
	"drift_doc_dead_symbol_reference": "reads markdown, which the corpus has none of",
	"drift_doc_outdated_count":        "reads markdown, which the corpus has none of",
	"fan_out_skew":                    "needs a function with many distinct receivers",
	"long_file":                       "no corpus file is long enough to matter, but every file is reported",
}

func TestEveryBuiltinRule_SameFindingsFromProjectionV4AndV6(t *testing.T) {
	v4db, v6db := parityCorpus(t, fixturedb.Leyline), parityCorpus(t, fixturedb.LeylineV6)
	require.NoError(t, ensureSmellQueryContext(v4db))
	require.NoError(t, ensureSmellQueryContext(v6db))
	for _, rule := range mustLoadEmbeddedRules() {
		t.Run(rule.ID, func(t *testing.T) {
			require.False(t, rule.PathShapedIDs, "a built-in rule must read node ids as opaque")
			run := func(db *fixturedb.FixtureDB) []smellFinding {
				out, err := runSmellRuleQuery(db, &rule, "", 1000)
				require.NoError(t, err)
				for i := range out {
					out[i].key = nil // a path on one side, a nid on the other, by design
				}
				return out
			}
			v4 := run(v4db)
			if _, exempt := parityExempt[rule.ID]; !exempt {
				require.NotEmpty(t, v4, "the corpus must give %s something to find, or parity is vacuous", rule.ID)
			}
			assert.Equal(t, v4, run(v6db))
		})
	}
}
