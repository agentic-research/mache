package smells

import (
	"fmt"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/lloschema"
)

// EnsureCanonicalViews installs every view a smell rule may read: the
// producer-translation vocabulary from internal/lloschema, then the three
// views derived FROM it for particular rule questions.
//
// The split is the layering. v_ast / v_nodes / v_defs / v_refs translate
// ley-line-open's physical schema and belong to that boundary, so `graph` can
// install them without depending on the smell engine (mache-be17ce).
// v_test_nodes, v_vendored_files and v_doc_refs answer questions only rules
// ask, and stay here.
//
// Installed as one call rather than at each rule-run site so every caller gets
// all of them — production and the 26 tests that build fixtures by hand. Rules
// reference the derived views unconditionally, so a caller that installed the
// vocabulary but not these produces "no such table" at query time.
func EnsureCanonicalViews(qg graph.RefsQuerier) error {
	if err := lloschema.EnsureViews(qg); err != nil {
		return err
	}

	hasAST, err := lloschema.TableHasColumn(qg, "_ast", "node_id")
	if err != nil {
		return fmt.Errorf("probe _ast: %w", err)
	}
	if hasAST {
		// Both tables are required: _ast supplies positions and kinds,
		// node_content the attribute's token. Fixtures carry one without the
		// other, and a half-present schema must degrade to "no test nodes".
		hasContent, cerr := lloschema.TableHasColumn(qg, "node_content", "token")
		if cerr != nil {
			return fmt.Errorf("probe node_content: %w", cerr)
		}
		hasAST = hasContent
	}
	if err := ensureTestNodesView(qg, hasAST); err != nil {
		return err
	}
	if err := ensureVendoredView(qg); err != nil {
		return err
	}

	hasRefsSourceID, err := lloschema.TableHasColumn(qg, "node_refs", "source_id")
	if err != nil {
		return fmt.Errorf("probe node_refs.source_id: %w", err)
	}
	return ensureDocRefsView(qg, hasRefsSourceID)
}
