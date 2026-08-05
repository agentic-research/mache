package graph

import (
	"fmt"
	"os/exec"

	"github.com/agentic-research/mache/internal/leyline"
)

// Build parses source with the pinned leyline binary and writes the result
// to output — the library equivalent of `mache build <source> <output>` with
// no --schema flag. That is the common case, and it is what Open expects: a
// raw leyline-parsed .db (nodes/node_defs/node_refs), which SQLiteGraph reads
// directly with no import/populate step (see Open's doc comment).
//
// The leyline binary is resolved the same way every mache entry point
// resolves it (PATH, if it matches the pin; else the version-namespaced
// cache; else auto-download, SHA-verified) — see leyline.ResolveBinary.
// Set MACHE_NO_LEYLINE to opt out of auto-download; Build then fails with a
// clear error instead of silently falling back to anything else, since
// leyline is mache's only parser.
//
// This does NOT run mache's --schema re-projection path (Engine + ASTWalker
// over a custom api.Topology) — that stays CLI-only for now. It also does
// not stamp the CLI's _mache_meta provenance table: mache_version/
// mache_commit there identify the mache BINARY that built the file, which a
// library caller isn't one of, and nothing in this package or Open reads
// that table.
func Build(source, output string) error {
	leylineBin, err := leyline.ResolveBinary(true)
	if err != nil {
		return fmt.Errorf("resolve leyline: %w", err)
	}
	leyline.RecordResolved(leylineBin, "resolved")

	cmd := exec.Command(leylineBin, "parse", source, "-o", output)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("leyline parse: %w\n%s", err, out)
	}
	return nil
}
