package smells

import (
	"fmt"

	"github.com/agentic-research/mache/graph"
)

// vendoredPrefixes are the path prefixes whose contents this project does not
// own. Today that is the snapshot corpus (ADR-0019): real third-party trees
// checked in verbatim to be PARSED, never to be edited.
//
// One list, because the alternative is the drift this codebase has already
// paid for once — dead_code, fan_out_skew and god_file each grew their own
// independently-written Go test exclusions, each correct for Go and silently
// wrong elsewhere, which is why v_test_nodes exists. A vendored-corpus
// predicate copied into every mean-relative rule would repeat that exactly.
var vendoredPrefixes = []string{
	"testdata/snapshots/",
}

// vendoredViewSQL builds `v_vendored_files`: the single definition of "this
// file is vendored corpus, not code this project owns".
//
// WHY THIS EXISTS (mache-f41b43). `find-smells --rule god_file` returned 26
// findings on main, and 24 were `testdata/snapshots/medium-rust-rosary/**` —
// vendored Rust that exists to be parsed. Nobody is going to refactor it, so
// the findings are noise; docs/smell-baseline.json was ~92% vendored fixtures.
//
// The second effect is worse than the noise. god_file fires at
// `count >= 10 AND count > 3 * mu`, and fan_out_skew has the same shape, so a
// large vendored corpus does not merely produce junk findings — it MOVES THE
// BAR for mache's own code by dragging the project mean. The rules already
// know this failure mode from a different source: fan_out_skew's body records
// markdown spans pulling mu from 9.79 to 5.08 and calls the result findings
// that "reflect a diluted mean, not a complexity change" (mache-50e939). Same
// mechanism, different input. Excluding vendored files from the FINDINGS but
// not from the MEAN would fix the noise and leave the threshold distortion in
// place, so both rules join this view before computing mu as well as after.
//
// A view rather than a build-time exclusion: the fixtures must stay in the
// projection, because other tests parse them. What is scoped is the
// population a smell rule judges, which is a question about the rule.
func vendoredViewSQL() string {
	// Built from vendoredPrefixes so the list above is the only place a new
	// vendored tree is declared.
	sql := `CREATE TEMP VIEW IF NOT EXISTS v_vendored_files AS
		SELECT DISTINCT COALESCE(source_file, '') AS source_file
		FROM nodes
		WHERE COALESCE(source_file, '') <> ''
		  AND (0`
	for _, p := range vendoredPrefixes {
		sql += fmt.Sprintf("\n\t\t   OR COALESCE(source_file, '') LIKE '%s%%'", p)
	}
	return sql + "\n\t\t)"
}

// ensureVendoredView installs v_vendored_files on this connection. Like the
// other canonical views it is a TEMP view, so it lives exactly as long as the
// connection that will query it.
func ensureVendoredView(qg graph.RefsQuerier) error {
	// The returned Rows MUST be closed even for DDL: RefsQuerier only exposes
	// a query method, and a fixture pinned to one connection (as
	// internal/fixturedb is, deliberately, so TEMP objects survive) blocks
	// forever on the next statement if this one is still holding it.
	rows, err := qg.QueryRefs(vendoredViewSQL())
	if err != nil {
		return fmt.Errorf("create v_vendored_files: %w", err)
	}
	return rows.Close()
}
