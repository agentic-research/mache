package graph

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/agentic-research/mache/api"
)

// What one record contributes to the direct path's scan: the directory and
// file entries its names render to, the record each directory with files is
// bound to, and the tokens its nodes define and reference. scanRoot streams
// the rows and calls collectPathEntries once per record.

type scanResult struct {
	entries  []pathEntry
	leafDirs []leafMapping
	// defs and refs are the tokens the record's nodes define and reference.
	defs []tokenAt
	refs []tokenAt
	// claimed holds every directory with files already bound to a record in
	// this scan. Unlike reset's slices it lives across records: it is how a
	// second record rendering the same directory is told apart.
	claimed map[string]bool
}

// reset empties the result for the next record, keeping its buffers.
func (r *scanResult) reset() {
	r.entries, r.leafDirs, r.defs, r.refs = r.entries[:0], r.leafDirs[:0], r.defs[:0], r.refs[:0]
}

// claim binds a directory with files to the record being scanned and
// returns its path: as rendered when no earlier record holds it, else with
// the first free `.2`, `.3`, ... suffix. That is the engine's rule for a
// construct whose name is taken (claimConstructID, with no source file to
// distinguish records), applied in the same record order, so both paths give
// a record the same ID. The direct path used to bind the second record to
// the first one's directory, so one of them was never served
// (mache-299113). Containers (no files) still merge: every record under
// `{{.area}}` shares the area's directory, on both paths.
func (r *scanResult) claim(path string) string {
	if !r.claimed[path] {
		r.claimed[path] = true
		return path
	}
	for n := 2; ; n++ {
		numbered := fmt.Sprintf("%s.%d", path, n)
		if !r.claimed[numbered] {
			r.claimed[numbered] = true
			log.Printf("[WARN] duplicate name %q: a second record renders the same directory — serving it as %q", path, numbered)
			return numbered
		}
	}
}

// tokenAt is a token and the directory node it belongs to.
type tokenAt struct {
	token string
	dir   string
}

type pathEntry struct {
	parent string
	child  string
}

type leafMapping struct {
	dirPath  string
	recordID string
}

// typedRefValues is the values refs templates render against: the fields
// they name, decoded from the JSON text the scan selected them as, so a list
// is a list and a number a number. Names keep rendering against the strings
// they always have. With no ref fields it is the name values.
func typedRefValues(nameValues map[string]any, refFieldPaths []string, cols []sql.NullString) map[string]any {
	if len(refFieldPaths) == 0 {
		return nameValues
	}
	values := make(map[string]any, len(refFieldPaths))
	for i, path := range refFieldPaths {
		var decoded any
		if cols[i].Valid {
			if err := json.Unmarshal([]byte(cols[i].String), &decoded); err != nil {
				decoded = nil
			}
		}
		setNestedField(values, path, decoded)
	}
	return values
}

// recordTokens adds a record's definitions and references to the graph's
// indexes.
func (g *SQLiteGraph) recordTokens(result *scanResult) {
	for _, d := range result.defs {
		_ = g.AddDef(d.token, d.dir)
	}
	for _, r := range result.refs {
		_ = g.AddRef(r.token, r.dir)
	}
}

// isTemplated reports whether a schema name is a template.
func isTemplated(name string) bool { return strings.Contains(name, "{{") }

// fileName is a leaf's name in one record's directory: rendered when the
// schema's name is a template, as the engine renders it, escaped to one
// segment, and UnnamedName when it renders empty. The direct path used to
// write a templated name verbatim, `{{.kind}}.txt`, and no lookup could
// reach it (mache-56ff85).
func (g *SQLiteGraph) fileName(f *api.Leaf, values map[string]any) string {
	if !isTemplated(f.Name) {
		return f.Name
	}
	rendered, err := g.render(f.Name, values)
	if err != nil {
		rendered = ""
	}
	name, _ := NameOrUnnamed(rendered)
	return SegmentEscape(name)
}

// collectPathEntries walks the schema children for one record, producing
// parent→child entries, leaf directory→recordID mappings, and the tokens each
// node defines and references. A directory with files defines its name, as
// the ingest engine has it; any node with refs templates references what they
// render to. refs used to be ignored here, so a data source served straight
// from its records answered no find_callers (mache-1bf0d8).
func (g *SQLiteGraph) collectPathEntries(level *schemaLevel, values, refValues map[string]any, parentPath, recordID string, result *scanResult) {
	for _, child := range level.children {
		rendered, err := g.render(child.nameRaw, values)
		if err != nil {
			continue
		}
		name, unnamed := NameOrUnnamed(rendered)

		childPath := parentPath + "/" + SegmentEscape(name)
		if len(child.files) > 0 {
			childPath = result.claim(childPath)
		}
		result.entries = append(result.entries, pathEntry{parent: parentPath, child: childPath})

		// Recurse into deeper directory levels
		if len(child.children) > 0 {
			g.collectPathEntries(child, values, refValues, childPath, recordID, result)
		}

		// Leaf directory: add file children and record mapping
		if len(child.files) > 0 {
			result.leafDirs = append(result.leafDirs, leafMapping{dirPath: childPath, recordID: recordID})
			for i := range child.files {
				filePath := childPath + "/" + g.fileName(&child.files[i], values)
				result.entries = append(result.entries, pathEntry{parent: childPath, child: filePath})
				if isTemplated(child.files[i].Name) {
					g.templatedLeaves.Store(filePath, &child.files[i])
				}
			}
			if !unnamed {
				for _, token := range DefTokensForName(name) {
					result.defs = append(result.defs, tokenAt{token: token, dir: childPath})
				}
			}
		}

		for _, refTmpl := range child.refs {
			rendered, err := g.render(refTmpl, refValues)
			if err != nil {
				continue
			}
			for _, token := range RefTokens(rendered) {
				result.refs = append(result.refs, tokenAt{token: token, dir: childPath})
			}
		}
	}
}
