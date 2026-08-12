package build

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

func schemaCoverageGaps(db *sql.DB, topology *api.Topology, source string, extraLanguages []string) ([]string, error) {
	languages := make(map[string]bool)
	for _, name := range extraLanguages {
		if name != "" {
			languages[name] = true
		}
	}
	var collect func([]api.Node)
	collect = func(nodes []api.Node) {
		for i := range nodes {
			if nodes[i].Language != "" {
				languages[nodes[i].Language] = true
			}
			collect(nodes[i].Children)
		}
	}
	collect(topology.Nodes)
	if len(languages) == 0 {
		return nil, nil
	}

	var gaps []string
	for name := range languages {
		language := lang.ForName(name)
		if language == nil || !sourceHasExtension(source, language.Extensions) {
			continue
		}
		parsed := false
		for _, extension := range language.Extensions {
			var exists int
			if err := db.QueryRow(
				`SELECT EXISTS(SELECT 1 FROM _ast WHERE source_id LIKE ?)`,
				"%"+extension,
			).Scan(&exists); err != nil {
				return nil, err
			}
			if exists == 1 {
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

func sourceHasExtension(root string, extensions []string) bool {
	found := false
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // unreadable entries are not evidence of a coverage gap
		}
		if entry.IsDir() {
			if path != root && ingest.ShouldSkipDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		for _, extension := range extensions {
			if strings.HasSuffix(path, extension) {
				found = true
				return filepath.SkipAll
			}
		}
		return nil
	})
	return found
}
