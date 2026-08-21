package cmd

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/agentic-research/mache/api"
	_ "modernc.org/sqlite"
)

// nodeTx is a write transaction that also knows the shape of the `nodes`
// table it is writing to.
//
// materializeVirtuals is handed whatever .db the caller points at, which may be
// a ley-line-open arena rather than a mache-built one. ley-line-open's
// projection-v4 makes nodes.parent_id a GENERATED column (mache-bc6ca3): SQLite
// rejects an INSERT that merely NAMES a generated column, and rejects it at
// PREPARE time, so the column list has to differ between the two shapes. The
// value does not — a generated parent_id is `id` with the trailing "/"+`name`
// removed, which is what every caller here already passes.
//
// The shape rides ON the transaction rather than beside it because the two must
// not come apart: a shape probed from one transaction and used to write through
// another would silently pick the wrong column list.
//
// *sql.Tx is EMBEDDED, not wrapped: Query/QueryRow/Commit/Rollback are promoted
// rather than restated. Hand-written one-line delegations are duplicate bodies
// that the duplicate_code gate correctly objects to, and they would have to be
// kept in step with a type this file does not own.
type nodeTx struct {
	*sql.Tx
	// derivedParent is true when nodes.parent_id is generated, so inserts
	// must omit it and let SQLite compute it.
	derivedParent bool
}

// beginNodeTx opens the transaction and probes the nodes shape together.
func beginNodeTx(db *sql.DB) (*nodeTx, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	return &nodeTx{Tx: tx, derivedParent: nodesParentIsGenerated(tx)}, nil
}

// nodesParentIsGenerated mirrors graph.ColumnIsGenerated for a *sql.Tx.
// pragma_table_xinfo.hidden: 2 = GENERATED VIRTUAL, 3 = GENERATED STORED.
// table_info would be useless here — it omits generated columns entirely, so
// it cannot distinguish "derived" from "absent".
func nodesParentIsGenerated(tx *sql.Tx) bool {
	var hidden int
	if err := tx.QueryRow(
		`SELECT hidden FROM pragma_table_xinfo('nodes') WHERE name = 'parent_id'`,
	).Scan(&hidden); err != nil {
		return false
	}
	return hidden == 2 || hidden == 3
}

// insertNode inserts one node row. orReplace selects INSERT OR REPLACE.
//
// It refuses a row whose name is not the last segment of its own id. Under a
// stored parent_id such a row is merely odd; under a derived one it silently
// takes the WRONG parent (usually the root), and a directory listing quietly
// goes empty rather than erroring. ley-line-open hit exactly that on a fixture
// with id='root/tricky', name='line1\nline2'. Checked on both shapes so the
// two paths cannot disagree about where a node lives.
func (n *nodeTx) insertNode(orReplace bool, id, parentID, name string,
	kind, size int, mtime int64, record any,
) error {
	if err := checkParentDerivable(id, parentID, name); err != nil {
		return err
	}
	verb := "INSERT INTO"
	if orReplace {
		verb = "INSERT OR REPLACE INTO"
	}
	if n.derivedParent {
		_, err := n.Exec(verb+
			` nodes (id, name, kind, size, mtime, record) VALUES (?, ?, ?, ?, ?, ?)`,
			id, name, kind, size, mtime, record)
		return err
	}
	_, err := n.Exec(verb+
		` nodes (id, parent_id, name, kind, size, mtime, record) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, parentID, name, kind, size, mtime, record)
	return err
}

// checkParentDerivable asserts the invariant ley-line-open's generated
// parent_id depends on: a node's id is either exactly its name (a root, parent
// "") or its parent's id followed by "/"+name. Returns an error naming the row
// rather than letting the mismatch through, because the failure it prevents is
// invisible — a wrong parent reads as an empty directory, not as an error.
func checkParentDerivable(id, parentID, name string) error {
	if parentID == "" {
		if id != name {
			return fmt.Errorf("node %q: root node name %q must equal its id "+
				"(parent_id is derived from id and name)", id, name)
		}
		return nil
	}
	if want := parentID + "/" + name; id != want {
		return fmt.Errorf("node %q: id must be parent %q + \"/\" + name %q = %q "+
			"(parent_id is derived from id and name)", id, parentID, name, want)
	}
	return nil
}

// materializeVirtuals adds virtual file nodes to the .db so that leyline's
// NFS mount can serve them without mache-specific runtime logic.
//
// Materialized files:
//   - _schema.json  (schema topology as JSON)
//   - PROMPT.txt    (if agentMode)
//   - callers/ dirs (from node_refs cross-references)
func materializeVirtuals(dbPath string, schema *api.Topology, agentMode bool) error {
	// Expand file_sets/include references before materialization.
	schema.ResolveIncludes()

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	nt, err := beginNodeTx(db)
	if err != nil {
		return err
	}
	defer func() { _ = nt.Rollback() }()

	now := time.Now().UnixNano()

	// 1. _schema.json at root
	schemaJSON, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal schema: %w", err)
	}
	if err := nt.insertNode(true,
		"_schema.json", "", "_schema.json", 0, len(schemaJSON), now, string(schemaJSON),
	); err != nil {
		return fmt.Errorf("insert _schema.json: %w", err)
	}

	// 2. PROMPT.txt if agent mode
	if agentMode {
		prompt := "# Mache Agent Mode\n\nThis mount was generated by mache for use with AI agents.\nRead source files under the function directories.\n"
		if err := nt.insertNode(true,
			"PROMPT.txt", "", "PROMPT.txt", 0, len(prompt), now, prompt,
		); err != nil {
			return fmt.Errorf("insert PROMPT.txt: %w", err)
		}
	}

	// 3. Materialize callers
	if err := materializeCallers(nt, now); err != nil {
		return fmt.Errorf("materialize callers: %w", err)
	}

	// 4. Materialize callees
	if err := materializeCallees(nt, now); err != nil {
		return fmt.Errorf("materialize callees: %w", err)
	}

	// 5. Materialize content-source files declared in the schema
	if err := materializeContentSources(nt, schema, now); err != nil {
		return fmt.Errorf("materialize content sources: %w", err)
	}

	// 6. Strip full content from _project_files/ (metadata-only in --out mode)
	if err := stripProjectFileContent(nt.Tx); err != nil {
		return fmt.Errorf("strip project file content: %w", err)
	}

	return nt.Commit()
}

// materializeCallers reads node_refs and creates callers/ directory nodes
// for each function directory that has callers.
//
// For a ref (token="HandleRequest", node_id="functions/ProcessOrder/source"),
// this creates:
//   - functions/HandleRequest/callers       (dir)
//   - functions/HandleRequest/callers/ProcessOrder (file, content = node_id)
func materializeCallers(nt *nodeTx, now int64) error {
	// Check if node_refs table exists
	var count int
	err := nt.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='node_refs'`).Scan(&count)
	if err != nil || count == 0 {
		return nil // no refs table, nothing to do
	}

	// Read all refs: token -> list of calling node IDs.
	// Skip the engine's '_file_level:%' sentinel rows (PR #270) —
	// they're synthetic caller_ids the engine uses to mark
	// file-level fn-value refs as alive without polluting
	// per-construct counts. Materializing them as caller entries
	// would surface internal state ('callers/path' from the
	// sentinel's path component) and store the sentinel string
	// itself as caller content. find_callers / search already
	// filter these; the materializer does so for the same reason.
	rows, err := nt.Query(`SELECT token, node_id FROM node_refs WHERE node_id NOT LIKE '_file_level:%'`)
	if err != nil {
		return fmt.Errorf("query node_refs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// Group by token
	callerMap := make(map[string][]string) // token -> []callerNodeID
	for rows.Next() {
		var token, nodeID string
		if err := rows.Scan(&token, &nodeID); err != nil {
			return err
		}
		callerMap[token] = append(callerMap[token], nodeID)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// For each token, find the matching directory node (via node_defs)
	// and create callers/ entries
	for token, callerNodeIDs := range callerMap {
		// Look up the directory that defines this token
		var dirID string
		err := nt.QueryRow(`SELECT node_id FROM node_defs WHERE token = ?`, token).Scan(&dirID)
		if err != nil {
			continue // no definition found, skip
		}

		// Create callers/ directory under the defining dir
		callersID := dirID + "/callers"

		// Check if callers/ dir already exists
		var exists int
		_ = nt.QueryRow(`SELECT COUNT(*) FROM nodes WHERE id = ?`, callersID).Scan(&exists)
		if exists == 0 {
			if err := nt.insertNode(false,
				callersID, dirID, "callers", 1, 0, now, nil,
			); err != nil {
				return fmt.Errorf("insert callers dir %s: %w", callersID, err)
			}
		}

		// Create an entry for each caller
		for _, callerNodeID := range callerNodeIDs {
			// Extract the caller's function name from the node_id path
			// e.g. "functions/ProcessOrder/source" -> "ProcessOrder"
			callerName := extractFuncName(callerNodeID)
			if callerName == "" {
				continue
			}

			entryID := callersID + "/" + callerName
			// Check if entry already exists
			var entryExists int
			_ = nt.QueryRow(`SELECT COUNT(*) FROM nodes WHERE id = ?`, entryID).Scan(&entryExists)
			if entryExists > 0 {
				continue
			}

			content := callerNodeID // content points to the calling node
			if err := nt.insertNode(false,
				entryID, callersID, callerName, 0, len(content), now, content,
			); err != nil {
				return fmt.Errorf("insert caller entry %s: %w", entryID, err)
			}
		}
	}

	return nil
}

// materializeCallees reads node_refs and creates callees/ directory nodes
// for each function directory that calls other functions.
// This is the inverse of materializeCallers: callers/ shows "who calls me",
// callees/ shows "who do I call".
//
// For a ref (token="FuncB", node_id="pkg/functions/FuncA"),
// with a def (token="FuncB", dir_id="pkg/functions/FuncB"),
// this creates:
//   - pkg/functions/FuncA/callees       (dir)
//   - pkg/functions/FuncA/callees/FuncB (file, content = dir_id of the callee)
func materializeCallees(nt *nodeTx, now int64) error {
	// Check if required tables exist
	var count int
	err := nt.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='node_refs'`).Scan(&count)
	if err != nil || count == 0 {
		return nil
	}
	err = nt.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='node_defs'`).Scan(&count)
	if err != nil || count == 0 {
		return nil
	}

	// Pre-load all defs: token → []dir_id (multiple constructs may define
	// the same token, e.g. Init in different packages).
	defsMap := make(map[string][]string)
	defRows, err := nt.Query(`SELECT token, node_id FROM node_defs`)
	if err != nil {
		return fmt.Errorf("query node_defs: %w", err)
	}
	for defRows.Next() {
		var token, dirID string
		if err := defRows.Scan(&token, &dirID); err != nil {
			_ = defRows.Close()
			return err
		}
		defsMap[token] = append(defsMap[token], dirID)
	}
	_ = defRows.Close()
	if err := defRows.Err(); err != nil {
		return err
	}

	// Read all refs and resolve callees via the pre-loaded defs map.
	// Skip sentinel rows (see materializeCallers above for the rationale).
	// extractCallerDir would resolve a sentinel node_id to itself;
	// the subsequent "exists in nodes" check filters it accidentally,
	// but we'd rather not depend on that — explicit is better, and
	// the SQL filter is cheaper than the per-row JOIN check.
	rows, err := nt.Query(`SELECT token, node_id FROM node_refs WHERE node_id NOT LIKE '_file_level:%'`)
	if err != nil {
		return fmt.Errorf("query node_refs: %w", err)
	}

	// Group by caller dir: callerDir -> list of (token, callee dir_id)
	type calleeInfo struct {
		token string
		dirID string
	}
	callerCallees := make(map[string][]calleeInfo) // callerDir -> callees

	for rows.Next() {
		var token, nodeID string
		if err := rows.Scan(&token, &nodeID); err != nil {
			_ = rows.Close()
			return err
		}

		// Find the caller's directory (parent of the referencing node)
		callerDir := extractCallerDir(nodeID)
		if callerDir == "" {
			continue
		}

		// Look up in pre-loaded map — emit one edge per definition
		dirIDs, ok := defsMap[token]
		if !ok {
			continue // no definition found
		}
		for _, calleeDirID := range dirIDs {
			callerCallees[callerDir] = append(callerCallees[callerDir], calleeInfo{token: token, dirID: calleeDirID})
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// Create callees/ directories and entries
	for callerDir, callees := range callerCallees {
		// Verify caller dir exists in nodes
		var exists int
		_ = nt.QueryRow(`SELECT COUNT(*) FROM nodes WHERE id = ?`, callerDir).Scan(&exists)
		if exists == 0 {
			continue
		}

		calleesID := callerDir + "/callees"

		// Create callees/ directory
		var dirExists int
		_ = nt.QueryRow(`SELECT COUNT(*) FROM nodes WHERE id = ?`, calleesID).Scan(&dirExists)
		if dirExists == 0 {
			if err := nt.insertNode(false,
				calleesID, callerDir, "callees", 1, 0, now, nil,
			); err != nil {
				return fmt.Errorf("insert callees dir %s: %w", calleesID, err)
			}
		}

		// Create entry for each callee
		for _, callee := range callees {
			calleeName := callee.token

			entryID := calleesID + "/" + calleeName
			var entryExists int
			_ = nt.QueryRow(`SELECT COUNT(*) FROM nodes WHERE id = ?`, entryID).Scan(&entryExists)
			if entryExists > 0 {
				continue
			}

			content := callee.dirID
			if err := nt.insertNode(false,
				entryID, calleesID, calleeName, 0, len(content), now, content,
			); err != nil {
				return fmt.Errorf("insert callee entry %s: %w", entryID, err)
			}
		}
	}

	return nil
}

// materializeContentSources walks the schema tree, finds all Leaf entries with
// ContentSource set, and creates file nodes by joining auxiliary tables with
// existing construct directories.
//
// Supported content sources:
//   - "lsp_hover"       → _lsp_hover table, hover_text column
//   - "lsp_diagnostics" → _lsp table, diagnostics column (non-null only)
//   - "lsp_defs"        → _lsp_defs table, grouped as JSON array per symbol
//   - "lsp_refs"        → _lsp_refs table, grouped as JSON array per symbol
//
// The matching logic: LSP node_ids use "symbols/{name}" format. We extract the
// leaf name and match it against directory names in the nodes table.
func materializeContentSources(nt *nodeTx, schema *api.Topology, now int64) error {
	// Collect all unique content_source values from the schema.
	sources := collectContentSources(schema)
	if len(sources) == 0 {
		return nil
	}

	// Build name→dirID index for matching LSP symbols to construct directories.
	nameToDir, err := buildNameToDirIndex(nt.Tx)
	if err != nil {
		return err
	}

	// For each content source, load data and create file nodes.
	for source, fileName := range sources {
		data, err := loadContentSource(nt.Tx, source)
		if err != nil {
			return fmt.Errorf("load content source %q: %w", source, err)
		}
		if data == nil {
			continue // table doesn't exist, skip
		}

		for symbolName, content := range data {
			dirs, ok := nameToDir[symbolName]
			if !ok {
				continue
			}
			for _, dirID := range dirs {
				fileID := dirID + "/" + fileName
				var exists int
				_ = nt.QueryRow(`SELECT COUNT(*) FROM nodes WHERE id = ?`, fileID).Scan(&exists)
				if exists > 0 {
					continue
				}
				if err := nt.insertNode(false,
					fileID, dirID, fileName, 0, len(content), now, content,
				); err != nil {
					return fmt.Errorf("insert %s: %w", fileID, err)
				}
			}
		}
	}

	return nil
}

// collectContentSources walks the schema tree and returns a map of
// content_source value → file name for all Leaf entries that use ContentSource.
func collectContentSources(schema *api.Topology) map[string]string {
	result := make(map[string]string)
	var walk func(nodes []api.Node)
	walk = func(nodes []api.Node) {
		for _, n := range nodes {
			for _, f := range n.Files {
				if f.ContentSource != "" {
					result[f.ContentSource] = f.Name
				}
			}
			walk(n.Children)
		}
	}
	walk(schema.Nodes)
	return result
}

// buildNameToDirIndex builds a map from directory leaf name to directory IDs.
func buildNameToDirIndex(tx *sql.Tx) (map[string][]string, error) {
	// kind = 1 → graph.NodeKindDir.
	rows, err := tx.Query(`SELECT id FROM nodes WHERE kind = 1`)
	if err != nil {
		return nil, fmt.Errorf("query dirs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	nameToDir := make(map[string][]string)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		parts := strings.Split(id, "/")
		name := parts[len(parts)-1]
		nameToDir[name] = append(nameToDir[name], id)
	}
	return nameToDir, rows.Err()
}

// lspSymbolLeafName extracts the leaf symbol name from an LSP node_id.
// "symbols/Model/memory_gb" → "memory_gb"
// "symbols/HandleRequest" → "HandleRequest"
func lspSymbolLeafName(nodeID string) string {
	s := strings.TrimPrefix(nodeID, "symbols/")
	parts := strings.Split(s, "/")
	return parts[len(parts)-1]
}

// loadContentSource queries the appropriate auxiliary table and returns
// a map of symbol leaf name → content string. Returns nil map (not error)
// if the backing table doesn't exist.
func loadContentSource(tx *sql.Tx, source string) (map[string]string, error) {
	switch source {
	case "lsp_hover":
		return loadLSPHover(tx)
	case "lsp_diagnostics":
		return loadLSPDiagnostics(tx)
	case "lsp_defs":
		return loadLSPDefs(tx)
	case "lsp_refs":
		return loadLSPRefs(tx)
	default:
		return nil, fmt.Errorf("unknown content source: %s", source)
	}
}

// tableExists checks if a table exists in the database.
func tableExists(tx *sql.Tx, name string) bool {
	var count int
	_ = tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&count)
	return count > 0
}

func loadLSPHover(tx *sql.Tx) (map[string]string, error) {
	if !tableExists(tx, "_lsp_hover") {
		return nil, nil
	}
	rows, err := tx.Query(`SELECT node_id, hover_text FROM _lsp_hover WHERE hover_text != ''`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	result := make(map[string]string)
	for rows.Next() {
		var nodeID, text string
		if err := rows.Scan(&nodeID, &text); err != nil {
			return nil, err
		}
		result[lspSymbolLeafName(nodeID)] = text
	}
	return result, rows.Err()
}

func loadLSPDiagnostics(tx *sql.Tx) (map[string]string, error) {
	if !tableExists(tx, "_lsp") {
		return nil, nil
	}
	rows, err := tx.Query(`SELECT node_id, diagnostics FROM _lsp WHERE diagnostics IS NOT NULL AND diagnostics != ''`)
	if err != nil {
		return nil, nil // column might not exist in older schemas
	}
	defer func() { _ = rows.Close() }()

	result := make(map[string]string)
	for rows.Next() {
		var nodeID, diag string
		if err := rows.Scan(&nodeID, &diag); err != nil {
			continue
		}
		result[lspSymbolLeafName(nodeID)] = diag
	}
	return result, rows.Err()
}

type lspLocation struct {
	URI       string `json:"uri"`
	StartLine int    `json:"start_line"`
	StartCol  int    `json:"start_col"`
	EndLine   int    `json:"end_line"`
	EndCol    int    `json:"end_col"`
}

func loadLSPLocations(tx *sql.Tx, table, query string) (map[string]string, error) {
	if !tableExists(tx, table) {
		return nil, nil
	}
	rows, err := tx.Query(query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	grouped := make(map[string][]lspLocation)
	for rows.Next() {
		var nodeID, uri string
		var sl, sc, el, ec int
		if err := rows.Scan(&nodeID, &uri, &sl, &sc, &el, &ec); err != nil {
			return nil, err
		}
		name := lspSymbolLeafName(nodeID)
		grouped[name] = append(grouped[name], lspLocation{URI: uri, StartLine: sl, StartCol: sc, EndLine: el, EndCol: ec})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	result := make(map[string]string)
	for name, locs := range grouped {
		data, _ := json.Marshal(locs)
		result[name] = string(data)
	}
	return result, nil
}

func loadLSPDefs(tx *sql.Tx) (map[string]string, error) {
	return loadLSPLocations(tx, "_lsp_defs",
		`SELECT node_id, def_uri, def_start_line, def_start_col, def_end_line, def_end_col FROM _lsp_defs`)
}

func loadLSPRefs(tx *sql.Tx) (map[string]string, error) {
	return loadLSPLocations(tx, "_lsp_refs",
		`SELECT node_id, ref_uri, ref_start_line, ref_start_col, ref_end_line, ref_end_col FROM _lsp_refs`)
}

// extractCallerDir gets the construct directory from a node_id.
// "pkg/functions/FuncA/source" -> "pkg/functions/FuncA"
// "pkg/functions/FuncA" -> "pkg/functions/FuncA"
//
// NOTE: The set of leaf names checked here (source, context, doc, callers,
// callees) is coupled to the leaf naming conventions used by the schema and
// the virtual directory materializers. If new leaf types are added, this
// function must be updated to match.
func extractCallerDir(nodeID string) (dirID string) {
	// Find the last slash. If there isn't one, mimic the original behavior
	// of returning an empty string.
	idx := strings.LastIndexByte(nodeID, '/')
	if idx < 0 {
		return dirID
	}

	// Use a switch for a clean, readable multi-match
	switch last := nodeID[idx+1:]; last {
	case "source", "context", "doc", "callers", "callees", "hover", "diagnostics", "definitions", "references":
		// Return the string up to (but not including) the last slash
		dirID = nodeID[:idx]
	default:
		dirID = nodeID
	}
	return dirID
}

// stripProjectFileContent removes full file content from _project_files/ file
// nodes, keeping only the directory structure and metadata (name, mtime).
// This prevents raw project files from bloating the --out DB.
func stripProjectFileContent(tx *sql.Tx) error {
	// kind = 0 → graph.NodeKindFile (file nodes only).
	_, err := tx.Exec(`UPDATE nodes SET record = NULL, size = 0 WHERE kind = 0 AND id LIKE '_project_files/%'`)
	return err
}

// extractFuncName pulls the function name from a node path like "functions/Foo/source".
// Returns the second-to-last path component (the function directory name).
func extractFuncName(nodeID string) string {
	// "functions/ProcessOrder/source" -> "ProcessOrder"
	// "types/MyStruct/source" -> "MyStruct"
	parts := strings.Split(nodeID, "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-2]
}
