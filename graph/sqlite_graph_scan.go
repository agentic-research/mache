package graph

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"text/template"
	"text/template/parse"

	machetemplate "github.com/agentic-research/mache/internal/template"
)

// ---------------------------------------------------------------------------
// Lazy scan: streaming pass over the source DB to build the directory tree
// for one root node. Resolves field references from name templates via
// json_extract pushed into SQLite, then renders parent→child path entries
// and leaf-directory record mappings.
// ---------------------------------------------------------------------------

// --- Scan types ---

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

// --- Field extraction from name templates ---

// collectTemplateFields walks a parsed template and records every field
// reference (e.g. `.item.cve.id` → "item.cve.id") into seen.
//
// This replaces a `\.(\w+(?:\.\w+)*)` regex over the raw template text. The
// regex scanned the WHOLE string, so a dot in literal text ("report.txt")
// became a phantom field path and an always-NULL json_extract column. Parsing
// sees only genuine field references.
func collectTemplateFields(n parse.Node, seen map[string]bool) {
	switch node := n.(type) {
	case nil:
		return
	case *parse.ListNode:
		if node == nil {
			return
		}
		for _, c := range node.Nodes {
			collectTemplateFields(c, seen)
		}
	case *parse.ActionNode:
		collectTemplateFields(node.Pipe, seen)
	case *parse.PipeNode:
		if node == nil {
			return
		}
		for _, c := range node.Cmds {
			collectTemplateFields(c, seen)
		}
	case *parse.CommandNode:
		for _, a := range node.Args {
			collectTemplateFields(a, seen)
		}
	case *parse.FieldNode:
		if len(node.Ident) > 0 {
			seen[strings.Join(node.Ident, ".")] = true
		}
	case *parse.ChainNode:
		collectTemplateFields(node.Node, seen)
	case *parse.IfNode:
		collectBranchFields(node.Pipe, node.List, node.ElseList, seen)
	case *parse.RangeNode:
		collectBranchFields(node.Pipe, node.List, node.ElseList, seen)
	case *parse.WithNode:
		collectBranchFields(node.Pipe, node.List, node.ElseList, seen)
	case *parse.TemplateNode:
		collectTemplateFields(node.Pipe, seen)
	}
}

func collectBranchFields(pipe *parse.PipeNode, list, elseList *parse.ListNode, seen map[string]bool) {
	collectTemplateFields(pipe, seen)
	collectTemplateFields(list, seen)
	collectTemplateFields(elseList, seen)
}

// collectNameTemplates gathers all dynamic name template strings from the schema tree.
func collectNameTemplates(level *schemaLevel) []string {
	var tmpls []string
	var walk func(*schemaLevel)
	walk = func(l *schemaLevel) {
		if !l.isStatic {
			tmpls = append(tmpls, l.nameRaw)
		}
		for _, c := range l.children {
			walk(c)
		}
	}
	walk(level)
	return tmpls
}

// collectRefTemplates gathers every refs template in the schema tree.
func collectRefTemplates(level *schemaLevel) []string {
	var tmpls []string
	var walk func(*schemaLevel)
	walk = func(l *schemaLevel) {
		tmpls = append(tmpls, l.refs...)
		for _, c := range l.children {
			walk(c)
		}
	}
	walk(level)
	return tmpls
}

// extractFieldPaths pulls dotted field references from Go templates.
// e.g. "{{slice .item.cve.id 4 8}}" → ["item.cve.id"]
func extractFieldPaths(templates []string) []string {
	seen := make(map[string]bool)
	for _, tmpl := range templates {
		// Parse with mache's func map so schema templates using slice/dig/
		// lookup/... parse cleanly; text/template supplies the builtins.
		// An unparseable template contributes nothing — it would fail at
		// render time anyway, so guessing at its fields is not useful.
		t, err := template.New("scan").Funcs(machetemplate.Funcs).Parse(tmpl)
		if err != nil || t.Tree == nil {
			continue
		}
		collectTemplateFields(t.Root, seen)
	}
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// buildScanQuery builds a SELECT using json_extract for only the fields
// needed by name templates. Avoids transferring and parsing full record JSON.
//
// refFieldPaths are the fields refs templates need. They are selected as JSON
// text (json_quote), after the name fields: a ref field is often a list, and
// a list has to reach its template as one, not as a string. A missing ref
// field is JSON null, so unlike a missing name field it does not drop the
// record.
func buildScanQuery(fieldPaths, refFieldPaths []string, tableName string) string {
	cols := make([]string, 0, len(fieldPaths)+len(refFieldPaths)+1)
	cols = append(cols, "id")
	for _, fp := range fieldPaths {
		cols = append(cols, fmt.Sprintf("json_extract(record, '$.%s')", fp))
	}
	for _, fp := range refFieldPaths {
		cols = append(cols, fmt.Sprintf("json_quote(json_extract(record, '$.%s'))", fp))
	}
	return "SELECT " + strings.Join(cols, ", ") + " FROM " + tableName
}

// setNestedField builds a nested map from a dotted path.
// e.g. setNestedField(m, "item.cve.id", "CVE-2024-0001")
//
//	→ m["item"]["cve"]["id"] = "CVE-2024-0001"
func setNestedField(m map[string]any, dottedPath string, value any) {
	parts := strings.Split(dottedPath, ".")
	current := m
	for i, part := range parts {
		if i == len(parts)-1 {
			current[part] = value
		} else {
			if v, ok := current[part]; ok {
				if nested, isMap := v.(map[string]any); isMap {
					current = nested
				} else {
					// Path conflict: intermediate value is not a map; overwrite it
					next := make(map[string]any)
					current[part] = next
					current = next
				}
			} else {
				next := make(map[string]any)
				current[part] = next
				current = next
			}
		}
	}
}

// --- Scan implementation ---

// flushBatchSize is the number of records between batch flushes to sync.Map.
// Keeps transient working-map memory bounded for large cross-reference scans.
const flushBatchSize = 50000

func (g *SQLiteGraph) ensureScanned(rootName string) error {
	val, _ := g.scanOnce.LoadOrStore(rootName, &sync.Once{})
	var err error
	val.(*sync.Once).Do(func() {
		err = g.scanRoot(rootName)
		if err != nil {
			g.scanErr.Store(rootName, err) // coverage:ignore
		} // coverage:ignore
	})
	if err != nil {
		return err // coverage:ignore
	} // coverage:ignore
	if v, ok := g.scanErr.Load(rootName); ok {
		return v.(error) // coverage:ignore
	} // coverage:ignore
	return nil
}

// scanRoot performs a single-pass streaming scan of all DB records to build the
// directory tree for one root node. Uses json_extract to push field extraction
// into SQLite, avoiding Go-side JSON parsing of the full record blob.
//
// Safety properties:
//   - Read-only transaction: snapshot isolation prevents partial results if the
//     source DB is being written to concurrently.
//   - Batch flush: every flushBatchSize records, accumulated slices are sorted,
//     deduped, and merged into sync.Map, then the working map is cleared. This
//     bounds transient memory for 10M+ node cross-reference scans.
//   - Error counting: scan/render failures are counted and logged rather than
//     silently swallowed, so data drops are visible.
//
// Why single-threaded: The previous worker-pool implementation (NumCPU goroutines +
// channels) was designed for CPU-bound template rendering, but profiling showed the
// bottleneck is SQLite I/O, not rendering. Name templates are simple field lookups
// (e.g. "{{.item.cve.id}}") that render in <1μs. The channel/goroutine overhead
// actually hurt throughput and introduced deadlock risk. If a future schema uses
// expensive template functions (regex, crypto), re-add parallelism — but measure first.
func (g *SQLiteGraph) scanRoot(rootName string) error {
	level := g.findRootLevel(rootName)
	if level == nil {
		return fmt.Errorf("root %q not found in schema", rootName)
	}

	// Analyze schema to find which fields the name templates need
	fieldPaths := extractFieldPaths(collectNameTemplates(level))
	refFieldPaths := extractFieldPaths(collectRefTemplates(level))
	query := buildScanQuery(fieldPaths, refFieldPaths, g.tableName)

	// Read-only transaction for snapshot consistency — if the source DB is
	// being written to during scan, we get a consistent point-in-time view.
	tx, err := g.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin scan tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // safe to ignore

	rows, err := tx.Query(query)
	if err != nil {
		return fmt.Errorf("scan query: %w", err)
	}
	defer func() { _ = rows.Close() }() // safe to ignore

	// Accumulate children as slices directly (no intermediate bool sets).
	// Flushed to sync.Map every flushBatchSize records to bound memory.
	childSlices := make(map[string][]string)
	recIDs := make(map[string]string)
	childSlices[rootName] = nil // ensure root exists even if DB is empty

	// Reusable per-row scan buffers — allocated once, reused every iteration
	nCols := len(fieldPaths) + len(refFieldPaths) + 1
	scanVals := make([]sql.NullString, nCols)
	scanPtrs := make([]any, nCols)
	for i := range scanVals {
		scanPtrs[i] = &scanVals[i]
	}
	fields := make([]string, len(fieldPaths))

	// Reusable result buffer for collectPathEntries
	result := scanResult{claimed: map[string]bool{}}

	count := 0
	scanErrs := 0
	nullSkips := 0
	for rows.Next() {
		if err := rows.Scan(scanPtrs...); err != nil {
			scanErrs++
			continue
		}

		// Check for NULL fields (records missing required template values)
		skip := false
		for i := range fieldPaths {
			if !scanVals[i+1].Valid {
				skip = true
				break
			}
			fields[i] = scanVals[i+1].String
		}
		if skip {
			nullSkips++
			continue
		}

		// Build minimal values map and render schema path tree
		values := make(map[string]any)
		for i, path := range fieldPaths {
			setNestedField(values, path, fields[i])
		}

		refValues := typedRefValues(values, refFieldPaths, scanVals[1+len(fieldPaths):])

		result.reset()
		g.collectPathEntries(level, values, refValues, rootName, scanVals[0].String, &result)
		g.recordTokens(&result)

		for _, e := range result.entries {
			childSlices[e.parent] = append(childSlices[e.parent], e.child)
		}
		for _, l := range result.leafDirs {
			recIDs[l.dirPath] = l.recordID
		}

		count++
		if count%100000 == 0 {
			log.Printf("Scanning %d records...", count)
		}

		// Batch flush: merge accumulated data into sync.Map to bound memory
		if count%flushBatchSize == 0 {
			flushChildSlices(childSlices, &g.dirChildren)
			for path, id := range recIDs {
				g.recordIDs.Store(path, id)
			}
			// Clear working maps but keep root entry
			childSlices = make(map[string][]string)
			childSlices[rootName] = nil
			recIDs = make(map[string]string)
		}
	}
	if count >= 100000 {
		log.Printf("Scanned %d records.", count)
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("scan rows: %w", err)
	}

	// Log skipped rows so data drops are visible
	if scanErrs > 0 || nullSkips > 0 {
		log.Printf("scan %q: %d records processed, %d scan errors, %d null-skipped",
			rootName, count, scanErrs, nullSkips)
	}

	// Final flush of remaining data
	flushChildSlices(childSlices, &g.dirChildren)
	for path, id := range recIDs {
		g.recordIDs.Store(path, id)
	}

	return nil
}

// flushChildSlices sorts, deduplicates, and merges accumulated child slices
// into the sync.Map. For keys already in the map (from a previous batch),
// the new children are merged and re-deduped.
func flushChildSlices(slices map[string][]string, target *sync.Map) {
	for parent, children := range slices {
		// Sort and compact this batch
		sort.Strings(children)
		j := 0
		for i, c := range children {
			if i == 0 || c != children[i-1] {
				children[j] = c
				j++
			}
		}
		deduped := children[:j]

		// Merge with any existing entries from prior batches
		if existing, ok := target.Load(parent); ok {
			prev := existing.([]string)
			deduped = mergeSortedDedup(prev, deduped)
		}

		target.Store(parent, deduped)
	}
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

// collectPathEntries walks the schema children for one record, producing
// parent→child entries, leaf directory→recordID mappings, and the tokens each
// node defines and references. A directory with files defines its name, as
// the ingest engine has it; any node with refs templates references what they
// render to. refs used to be ignored here, so a data source served straight
// from its records answered no find_callers (mache-1bf0d8).
func (g *SQLiteGraph) collectPathEntries(level *schemaLevel, values, refValues map[string]any, parentPath, recordID string, result *scanResult) {
	for _, child := range level.children {
		name, err := g.render(child.nameRaw, values)
		if err != nil || name == "" {
			continue
		}

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
			for _, f := range child.files {
				result.entries = append(result.entries, pathEntry{parent: childPath, child: childPath + "/" + f.Name})
			}
			for _, token := range DefTokensForName(name) {
				result.defs = append(result.defs, tokenAt{token: token, dir: childPath})
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

func (g *SQLiteGraph) findRootLevel(name string) *schemaLevel {
	for _, l := range g.levels {
		if l.isStatic && l.staticName == name {
			return l
		}
	}
	return nil
}

// isChild checks whether childPath appears in the cached children of parentPath.
func (g *SQLiteGraph) isChild(parentPath, childPath string) bool {
	v, ok := g.dirChildren.Load(parentPath)
	if !ok {
		return false
	}
	// Binary search on sorted children
	children := v.([]string)
	i := sort.SearchStrings(children, childPath)
	return i < len(children) && children[i] == childPath
}
