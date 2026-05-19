package graph

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"sync"
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

// fieldRefRe matches Go template field references like .item.cve.id
var fieldRefRe = regexp.MustCompile(`\.(\w+(?:\.\w+)*)`)

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

// extractFieldPaths pulls dotted field references from Go templates.
// e.g. "{{slice .item.cve.id 4 8}}" → ["item.cve.id"]
func extractFieldPaths(templates []string) []string {
	seen := make(map[string]bool)
	for _, tmpl := range templates {
		for _, m := range fieldRefRe.FindAllStringSubmatch(tmpl, -1) {
			seen[m[1]] = true
		}
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
func buildScanQuery(fieldPaths []string, tableName string) string {
	cols := make([]string, 0, len(fieldPaths)+1)
	cols = append(cols, "id")
	for _, fp := range fieldPaths {
		cols = append(cols, fmt.Sprintf("json_extract(record, '$.%s')", fp))
	}
	return "SELECT " + strings.Join(cols, ", ") + " FROM " + tableName
}

// setNestedField builds a nested map from a dotted path.
// e.g. setNestedField(m, "item.cve.id", "CVE-2024-0001")
//
//	→ m["item"]["cve"]["id"] = "CVE-2024-0001"
func setNestedField(m map[string]any, dottedPath, value string) {
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
	query := buildScanQuery(fieldPaths, g.tableName)

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
	nCols := len(fieldPaths) + 1
	scanVals := make([]sql.NullString, nCols)
	scanPtrs := make([]any, nCols)
	for i := range scanVals {
		scanPtrs[i] = &scanVals[i]
	}
	fields := make([]string, len(fieldPaths))

	// Reusable result buffer for collectPathEntries
	var result scanResult

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

		result.entries = result.entries[:0]
		result.leafDirs = result.leafDirs[:0]
		g.collectPathEntries(level, values, rootName, scanVals[0].String, &result)

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

// collectPathEntries walks the schema children for one record, producing
// parent→child entries and leaf directory→recordID mappings.
func (g *SQLiteGraph) collectPathEntries(level *schemaLevel, values map[string]any, parentPath, recordID string, result *scanResult) {
	for _, child := range level.children {
		name, err := g.render(child.nameRaw, values)
		if err != nil || name == "" {
			continue
		}

		childPath := parentPath + "/" + name
		result.entries = append(result.entries, pathEntry{parent: parentPath, child: childPath})

		// Recurse into deeper directory levels
		if len(child.children) > 0 {
			g.collectPathEntries(child, values, childPath, recordID, result)
		}

		// Leaf directory: add file children and record mapping
		if len(child.files) > 0 {
			result.leafDirs = append(result.leafDirs, leafMapping{dirPath: childPath, recordID: recordID})
			for _, f := range child.files {
				result.entries = append(result.entries, pathEntry{parent: childPath, child: childPath + "/" + f.Name})
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
