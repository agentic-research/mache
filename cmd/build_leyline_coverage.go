package cmd

import (
	"database/sql"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/lang"
)

// leylineSchemaCoverageGaps reports schema languages whose source files
// exist under source but produced ZERO `_ast` rows in the leyline parse —
// i.e. the schema will project hollow category dirs because leyline has no
// grammar for that language yet (leyline v0.7.5 parses ~11 of the 28
// registry languages). Before schema-on-leyline (mache-73b885) this failure
// was loud: the auto path warned and the explicit path errored. The guard
// restores that loudness with an accurate diagnosis: warnIfEmptyBuild can't
// catch it because the empty category dirs still count as nodes.
//
// A schema with no Language hints returns nil (nothing attributable — data
// schemas over JSON/SQLite never reach the leyline path anyway).
func leylineSchemaCoverageGaps(db *sql.DB, schema *api.Topology, source string) ([]string, error) {
	langs := map[string]bool{}
	var collect func(nodes []api.Node)
	collect = func(nodes []api.Node) {
		for i := range nodes {
			if nodes[i].Language != "" {
				langs[nodes[i].Language] = true
			}
			collect(nodes[i].Children)
		}
	}
	collect(schema.Nodes)
	if len(langs) == 0 {
		return nil, nil
	}

	var gaps []string
	for name := range langs {
		l := lang.ForName(name)
		if l == nil {
			continue // unknown language string; the engine ignores it too
		}
		if !sourceHasExtension(source, l.Extensions) {
			continue // no files of this language — nothing to project, not a gap
		}
		parsed := false
		for _, ext := range l.Extensions {
			var one int
			err := db.QueryRow(
				`SELECT EXISTS(SELECT 1 FROM _ast WHERE source_id LIKE ?)`,
				"%"+ext,
			).Scan(&one)
			if err != nil {
				return nil, err
			}
			if one == 1 {
				parsed = true
				break
			}
		}
		if !parsed {
			gaps = append(gaps, name)
		}
	}
	sort.Strings(gaps)
	return gaps, nil
}

// sourceHasExtension reports whether any non-skipped file under root
// carries one of the extensions. Stops at the first hit.
func sourceHasExtension(root string, exts []string) bool {
	found := false
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // unreadable entries can't be evidence either way
		}
		if d.IsDir() {
			if path != root && ingest.ShouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		for _, ext := range exts {
			if strings.HasSuffix(path, ext) {
				found = true
				return filepath.SkipAll
			}
		}
		return nil
	})
	return found
}
