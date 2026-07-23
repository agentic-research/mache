# Node Properties → `props` Column Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move node Properties out of the overloaded `record` column into a dedicated `props JSON` column, stored as real nested JSON so `json_extract(props,'$.lang')` works.

**Architecture:** Introduce an accessor seam (`PropString`/`PropRaw`) over `Node.Properties` first, so the later type change touches one file instead of eleven. Then flip `map[string][]byte` → `map[string]json.RawMessage`, add the column, and gate old mache-written `.db` files behind a producer-scoped refusal.

**Tech Stack:** Go 1.25, `modernc.org/sqlite` (pure Go), `stretchr/testify` (assert/require), Task runner.

**Spec:** `docs/design/specs/2026-07-22-node-properties-props-column-design.md`

## Global Constraints

- **Pure Go — no CGO.** ADR-0006/ADR-0012 hard invariant. Nothing in this plan may add a C dependency.
- **Formatter:** gofumpt (`task fmt`). Pre-commit hooks enforce it; a commit will be rejected otherwise.
- **Tests:** `task test` (not bare `go test`) — env vars are required. Single test: `task test -- -run TestName`.
- **Full gate before push:** `task ci`. The pre-push hook runs it; do not bypass.
- **Commit messages** start with `[mache-90b89b]`. **No co-author lines.**
- **Staging:** never `git add -A` / `git add .` / `git commit -a`. Stage explicit paths.
- **Six property keys exist**, all set in `engine_walk.go`: `lang`, `pkg`, `imports`, `location`, `ast_source_id`, `ast_scope_id`. `imports` is the only object-valued one; the other five are plain strings.

______________________________________________________________________

## File Structure

| File                                   | Responsibility                                                                                                                                |
| -------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------- |
| `internal/graph/props.go`              | **New.** The accessor seam: `PropString`, `SetPropString`, `PropRaw`, `SetPropRaw`. The only file that knows how a property value is encoded. |
| `internal/graph/props_test.go`         | **New.** Unit tests for the seam, including the quoting contract.                                                                             |
| `internal/graph/props_compat.go`       | **New.** `RequireProps` — the producer-scoped refusal.                                                                                        |
| `internal/graph/props_compat_test.go`  | **New.** The three-producer matrix from spec §2.3.                                                                                            |
| `internal/graph/graph.go:61`           | Type change on `Node.Properties`.                                                                                                             |
| `internal/ingest/sqlite_writer.go`     | Schema (`CREATE TABLE` + `ALTER`), `INSERT` column list, write `props`, stop writing Properties into `record`.                                |
| `internal/graph/nodes_table_reader.go` | Read `props`; drop the `CASE WHEN kind = NodeKindDir` workaround.                                                                             |
| `internal/graph/sqlite_graph.go:152`   | Call `RequireProps` in `OpenSQLiteGraph`.                                                                                                     |
| `internal/graph/writable_graph.go:55`  | Call `RequireProps` in the writable constructor.                                                                                              |
| `internal/graph/props_sql_test.go`     | **New.** The SQL-queryability tests — the point of the change.                                                                                |
| Consumer sites (7 files)               | Swap direct map reads for the seam.                                                                                                           |

______________________________________________________________________

## Task 1: Introduce the accessor seam (no behavior change)

Isolates every consumer behind four functions **while `Properties` is still `map[string][]byte`**, so Task 2's type flip is a small, safe diff. This task changes no behavior and must leave every existing test green.

**Files:**

- Create: `internal/graph/props.go`
- Create: `internal/graph/props_test.go`
- Modify: `internal/vfs/location.go:28,46,59`
- Modify: `internal/graph/composite.go:502`
- Modify: `internal/graph/memstore_callees.go:29,77,85,86`
- Modify: `internal/graph/sqlite_graph_callees.go:51`
- Modify: `internal/ingest/engine_walk.go:257,261,273,285,417,450,451`
- Modify: `cmd/serve_handler_read_file.go:45`
- Modify: `cmd/serve_handler_list_directory.go:93`

**Interfaces:**

- Consumes: nothing (first task).

- Produces: `graph.PropString(n *Node, key string) string`, `graph.SetPropString(n *Node, key, value string)`, `graph.PropRaw(n *Node, key string) []byte`, `graph.SetPropRaw(n *Node, key string, raw []byte)`.

- [ ] **Step 1: Write the failing test**

Create `internal/graph/props_test.go`:

```go
package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPropStringRoundTrip(t *testing.T) {
	n := &Node{ID: "pkg/functions/Foo"}
	SetPropString(n, "lang", "go")
	assert.Equal(t, "go", PropString(n, "lang"))
}

func TestPropStringAbsentAndNilSafe(t *testing.T) {
	assert.Equal(t, "", PropString(nil, "lang"), "nil node must not panic")
	assert.Equal(t, "", PropString(&Node{}, "lang"), "nil Properties map must not panic")

	n := &Node{}
	SetPropString(n, "pkg", "main")
	assert.Equal(t, "", PropString(n, "lang"), "absent key returns empty")
}

func TestPropRawPreservesObjectJSON(t *testing.T) {
	n := &Node{}
	SetPropRaw(n, "imports", []byte(`{"fmt":"fmt"}`))
	assert.JSONEq(t, `{"fmt":"fmt"}`, string(PropRaw(n, "imports")),
		"object-valued properties must survive byte-for-byte as JSON")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `task test -- -run 'TestProp' ./internal/graph/`

Expected: FAIL — `undefined: SetPropString`, `undefined: PropString`, `undefined: SetPropRaw`, `undefined: PropRaw`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/graph/props.go`:

```go
package graph

// Node Properties are accessed exclusively through this file. Keeping the
// encoding behind four functions is what lets the underlying map's value type
// change (mache-90b89b) without touching every consumer.

// PropString returns a string-valued property, or "" when the node, the map,
// or the key is absent.
func PropString(n *Node, key string) string {
	return string(PropRaw(n, key))
}

// SetPropString sets a string-valued property, allocating Properties if nil.
func SetPropString(n *Node, key, value string) {
	SetPropRaw(n, key, []byte(value))
}

// PropRaw returns a property's raw bytes. For object-valued properties such as
// "imports" these bytes are JSON; for the rest they are a plain string.
func PropRaw(n *Node, key string) []byte {
	if n == nil || n.Properties == nil {
		return nil
	}
	return n.Properties[key]
}

// SetPropRaw sets a property's raw bytes, allocating Properties if nil.
func SetPropRaw(n *Node, key string, raw []byte) {
	if n.Properties == nil {
		n.Properties = make(map[string][]byte)
	}
	n.Properties[key] = raw
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `task test -- -run 'TestProp' ./internal/graph/`

Expected: PASS (3 tests).

- [ ] **Step 5: Migrate every consumer to the seam**

`internal/graph/composite.go:502` — replace:

```go
	if v, ok := node.Properties["lang"]; ok {
		langName = string(v)
	}
```

with:

```go
	langName = PropString(node, "lang")
```

`internal/graph/memstore_callees.go:29` — replace `raw, ok := node.Properties["imports"]` / `if !ok || len(raw) == 0 {` with:

```go
	raw := PropRaw(node, "imports")
	if len(raw) == 0 {
```

`internal/graph/memstore_callees.go:77` — replace the `if v, ok := node.Properties["lang"]; ok { langName = string(v) }` block with `langName = PropString(node, "lang")`.

`internal/graph/memstore_callees.go:85-88` — replace:

```go
		astSrcID, hasSrc := node.Properties["ast_source_id"]
		astScopeID, hasScope := node.Properties["ast_scope_id"]
		if hasSrc && hasScope && len(astSrcID) > 0 && len(astScopeID) > 0 {
			calls, err := s.scopedExtractor(string(astSrcID), string(astScopeID), langName)
```

with:

```go
		astSrcID := PropString(node, "ast_source_id")
		astScopeID := PropString(node, "ast_scope_id")
		if astSrcID != "" && astScopeID != "" {
			calls, err := s.scopedExtractor(astSrcID, astScopeID, langName)
```

`internal/vfs/location.go:28` and `:46` — replace `loc, ok := node.Properties["location"]` / `if !ok || len(loc) == 0 {` with:

```go
	loc := graph.PropString(node, "location")
	if loc == "" {
```

and use `loc` as a `string` from there (drop any `string(loc)` conversions). At `:59` replace the condition with `if loc := graph.PropString(node, "location"); loc != "" {`.

`cmd/serve_handler_read_file.go:44-45` — replace:

```go
		if parent, err := g.GetNode(parentDir); err == nil && parent.Properties != nil {
			if loc, ok := parent.Properties["location"]; ok && len(loc) > 0 {
```

with:

```go
		if parent, err := g.GetNode(parentDir); err == nil {
			if loc := graph.PropString(parent, "location"); loc != "" {
```

`cmd/serve_handler_list_directory.go:92-93` — same substitution against `node`.

`internal/graph/sqlite_graph_callees.go:51` — apply the same `PropString(node, "lang")` / `PropString(node, "ast_source_id")` / `PropString(node, "ast_scope_id")` substitutions used in `memstore_callees.go`.

`internal/ingest/engine_walk.go` — replace the six write sites. Note this file is in package `ingest`, so calls are qualified. At `:254-257`:

```go
			graph.SetPropString(node, "lang", l)
```

At `:261`: `graph.SetPropString(node, "pkg", pkg)`. At `:269-273`: `graph.SetPropRaw(node, "imports", importJSON)`. At `:285`: `if pkg := graph.PropString(node, "pkg"); pkg != "" {`. At `:410-417`: `graph.SetPropString(node, "location", fmt.Sprintf("%s:%d:%d", relPath, startLine, endLine))`. At `:447-451`: `graph.SetPropString(node, "ast_source_id", srcID)` and `graph.SetPropString(node, "ast_scope_id", scopeID)`.

Delete the now-dead `if node.Properties == nil { node.Properties = make(...) }` guards at `:254`, `:269`, `:410`, `:447` — `SetPropString`/`SetPropRaw` allocate.

- [ ] **Step 6: Verify no direct map access remains**

Run: `rg 'Properties\[' --type go -g '!*_test.go' internal/ cmd/`

Expected: **only** `internal/graph/props.go`. Any other hit is an unmigrated site — migrate it before continuing.

- [ ] **Step 7: Run the full suite — this is a pure refactor**

Run: `task test`

Expected: PASS, with no test modified other than the new `props_test.go`. If an existing test fails, the refactor changed behavior and must be corrected — do not edit the test.

- [ ] **Step 8: Commit**

```bash
task fmt
git add internal/graph/props.go internal/graph/props_test.go \
        internal/graph/composite.go internal/graph/memstore_callees.go \
        internal/graph/sqlite_graph_callees.go internal/vfs/location.go \
        internal/ingest/engine_walk.go \
        cmd/serve_handler_read_file.go cmd/serve_handler_list_directory.go
git commit -m "[mache-90b89b] refactor(graph): route all Properties access through an accessor seam

Pure refactor, no behavior change. PropString/PropRaw and their setters become
the only code that knows how a property value is encoded, so the value-type
change that follows touches one file instead of eleven."
```

______________________________________________________________________

## Task 2: Flip the value type to `json.RawMessage`

**Files:**

- Modify: `internal/graph/graph.go:61`
- Modify: `internal/graph/props.go`
- Modify: `internal/graph/props_test.go`

**Interfaces:**

- Consumes: the seam from Task 1.

- Produces: `Node.Properties` is `map[string]json.RawMessage`; `PropString` returns the **unquoted** string; `PropRaw` returns the raw JSON.

- [ ] **Step 1: Write the failing test**

Append to `internal/graph/props_test.go`:

```go
func TestPropStringIsStoredAsQuotedJSON(t *testing.T) {
	n := &Node{}
	SetPropString(n, "lang", "go")

	// The stored form must be JSON, not the bare bytes — this is what makes
	// json_extract(props,'$.lang') return `go` instead of `Z28=`.
	assert.Equal(t, `"go"`, string(n.Properties["lang"]),
		"string properties must be stored as JSON strings")
	assert.Equal(t, "go", PropString(n, "lang"), "and must read back unquoted")
}

func TestPropertiesMarshalWithoutBase64(t *testing.T) {
	n := &Node{}
	SetPropString(n, "lang", "go")
	SetPropRaw(n, "imports", []byte(`{"fmt":"fmt"}`))

	out, err := json.Marshal(n.Properties)
	require.NoError(t, err)

	assert.Contains(t, string(out), `"lang":"go"`)
	assert.NotContains(t, string(out), "Z28=", "base64 of \"go\" must not appear")
	assert.Contains(t, string(out), `"imports":{"fmt":"fmt"}`,
		"imports must nest as a real object, not a base64 string")
}

func TestPropStringOnNonStringValueReturnsEmpty(t *testing.T) {
	n := &Node{}
	SetPropRaw(n, "imports", []byte(`{"fmt":"fmt"}`))
	assert.Equal(t, "", PropString(n, "imports"),
		"an object-valued property is not a string property")
}
```

Add `"encoding/json"` and `"github.com/stretchr/testify/require"` to the test imports.

- [ ] **Step 2: Run test to verify it fails**

Run: `task test -- -run 'TestPropStringIsStoredAsQuotedJSON|TestPropertiesMarshalWithoutBase64' ./internal/graph/`

Expected: FAIL — stored form is `go` not `"go"`, and the marshal contains `Z28=`.

- [ ] **Step 3: Change the type**

`internal/graph/graph.go:61`:

```go
	Properties map[string]json.RawMessage // Metadata / extended attributes
```

Add `"encoding/json"` to that file's imports if absent.

- [ ] **Step 4: Update the seam**

Replace the bodies in `internal/graph/props.go`:

```go
package graph

import "encoding/json"

// Node Properties are accessed exclusively through this file. Values are JSON:
// a string property is stored as a JSON string (`"go"`, not `go`), so the map
// marshals into a `props` column that json_extract can read (mache-90b89b).
// The previous map[string][]byte base64'd every value and double-encoded the
// one that was already JSON.

// PropString returns a string-valued property, or "" when the node, the map, or
// the key is absent, or the value is not a JSON string.
func PropString(n *Node, key string) string {
	raw := PropRaw(n, key)
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// SetPropString sets a string-valued property, allocating Properties if nil.
func SetPropString(n *Node, key, value string) {
	b, err := json.Marshal(value)
	if err != nil {
		return // coverage:ignore — marshaling a string cannot fail
	}
	SetPropRaw(n, key, b)
}

// PropRaw returns a property's raw JSON bytes.
func PropRaw(n *Node, key string) []byte {
	if n == nil || n.Properties == nil {
		return nil
	}
	return n.Properties[key]
}

// SetPropRaw sets a property's raw JSON bytes, allocating Properties if nil.
// The caller is responsible for `raw` being valid JSON.
func SetPropRaw(n *Node, key string, raw []byte) {
	if n.Properties == nil {
		n.Properties = make(map[string]json.RawMessage)
	}
	n.Properties[key] = raw
}
```

- [ ] **Step 5: Fix the two legacy unmarshal sites so the package compiles**

Both still declare `map[string][]byte`. In `internal/graph/nodes_table_reader.go` (~line 141) and `internal/ingest/sqlite_writer.go` (~line 620), change the local declaration:

```go
		var p map[string]json.RawMessage
```

These read the *old* on-disk format and are deleted in Task 4 — this step only keeps the build green.

- [ ] **Step 6: Run the full suite**

Run: `task test`

Expected: PASS. Any failure here is a consumer that bypassed the seam in Task 1 — fix the consumer, not the test.

- [ ] **Step 7: Commit**

```bash
task fmt
git add internal/graph/graph.go internal/graph/props.go internal/graph/props_test.go \
        internal/graph/nodes_table_reader.go internal/ingest/sqlite_writer.go
git commit -m "[mache-90b89b] refactor(graph): Properties values are json.RawMessage

map[string][]byte base64'd every value and double-encoded imports, which was
already JSON. RawMessage nests correctly, so a string property stores as \"go\"
and imports stores as a real object."
```

______________________________________________________________________

## Task 3: Add the `props` column and write to it

**Files:**

- Modify: `internal/ingest/sqlite_writer.go:55-70` (schema), `:143` (ALTER), `:171-172` (INSERT), `:445-452` (record/props split), `:463-473` (Exec args)
- Create: `internal/ingest/props_column_test.go`

**Interfaces:**

- Consumes: `graph.Node.Properties` as `map[string]json.RawMessage` (Task 2).

- Produces: a `nodes.props` column holding the marshaled Properties; `nodes.record` no longer holds Properties.

- [ ] **Step 1: Write the failing test**

Create `internal/ingest/props_column_test.go`:

```go
package ingest

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/graph"
)

// writeOneDirNode builds a .db containing a single dir node carrying
// lang + imports, and returns its path.
func writeOneDirNode(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "props.db")
	w, err := NewSQLiteWriter(dbPath)
	require.NoError(t, err)

	n := &graph.Node{ID: "pkg", ModTime: time.Unix(0, 0)}
	n.Mode = 0o555 | 0x80000000 // dir bit; ContentSize()==0
	graph.SetPropString(n, "lang", "go")
	graph.SetPropRaw(n, "imports", []byte(`{"fmt":"fmt"}`))
	w.AddNode(n)
	require.NoError(t, w.Close())
	return dbPath
}

func TestPropsColumnIsQueryableJSON(t *testing.T) {
	db, err := sql.Open("sqlite", writeOneDirNode(t)+"?mode=ro")
	require.NoError(t, err)
	defer db.Close()

	var lang string
	require.NoError(t, db.QueryRow(
		`SELECT json_extract(props,'$.lang') FROM nodes WHERE id='pkg'`).Scan(&lang))
	assert.Equal(t, "go", lang, "must be the value, not its base64")

	var importsType string
	require.NoError(t, db.QueryRow(
		`SELECT json_type(props,'$.imports') FROM nodes WHERE id='pkg'`).Scan(&importsType))
	assert.Equal(t, "object", importsType,
		"imports must be a nested object, not a base64 text blob")
}

func TestRecordNoLongerCarriesProperties(t *testing.T) {
	db, err := sql.Open("sqlite", writeOneDirNode(t)+"?mode=ro")
	require.NoError(t, err)
	defer db.Close()

	var record sql.NullString
	require.NoError(t, db.QueryRow(`SELECT record FROM nodes WHERE id='pkg'`).Scan(&record))
	assert.False(t, record.Valid && record.String != "",
		"a dir node with no inline Data must leave record NULL; got %q", record.String)
}

func TestPropsStoresNoBase64(t *testing.T) {
	db, err := sql.Open("sqlite", writeOneDirNode(t)+"?mode=ro")
	require.NoError(t, err)
	defer db.Close()

	var props string
	require.NoError(t, db.QueryRow(`SELECT props FROM nodes WHERE id='pkg'`).Scan(&props))
	assert.Contains(t, props, `"lang":"go"`)
	assert.NotContains(t, props, "Z28=", "base64 of \"go\" indicates a revert to map[string][]byte")

	var round map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(props), &round))
	assert.JSONEq(t, `{"fmt":"fmt"}`, string(round["imports"]))
}
```

Check `NewSQLiteWriter`'s real signature and the correct way to mark a dir node before writing this file; mirror an existing writer test in `internal/ingest/` rather than inventing a construction.

- [ ] **Step 2: Run test to verify it fails**

Run: `task test -- -run 'TestPropsColumn|TestRecordNoLonger|TestPropsStoresNoBase64' ./internal/ingest/`

Expected: FAIL — `no such column: props`.

- [ ] **Step 3: Add the column to the schema**

`internal/ingest/sqlite_writer.go`, in the `CREATE TABLE IF NOT EXISTS nodes` block, after `context BLOB`:

```sql
		context BLOB,
		-- props holds the node's Properties (lang/pkg/imports/location/ast_*)
		-- as real nested JSON so json_extract(props,'$.lang') is queryable.
		-- Previously these were base64'd into `record`, which also made that
		-- column mean three different things (mache-90b89b).
		props JSON
```

- [ ] **Step 4: Add the backward-compat ALTER**

Immediately after the existing `context` ALTER at `:143`:

```go
	if !graph.ColumnExists(db, "nodes", "props") {
		if _, err := db.Exec(`ALTER TABLE nodes ADD COLUMN props JSON`); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("add nodes.props column: %w", err)
		}
	}
```

- [ ] **Step 5: Widen the INSERT**

`:171-172`:

```go
		INSERT OR REPLACE INTO nodes (id, parent_id, name, kind, size, mtime, record_id, record, source_file, context, props)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
```

- [ ] **Step 6: Split record from props at the write site**

Replace `:445-452`:

```go
	// 4. Record content: inline rendered file content only. Properties now
	// have their own column, so `record` means one of two things (a source
	// data record, or inline content) instead of three (mache-90b89b).
	var record []byte
	if n.Data != nil {
		record = n.Data
	}

	// 4b. Properties → props, as real nested JSON.
	var props []byte
	if len(n.Properties) > 0 {
		props, _ = json.Marshal(n.Properties)
	}
```

Then add `props` as the final argument to `w.stmtNode.Exec(...)` at `:463`, after `n.Context`.

- [ ] **Step 7: Run test to verify it passes**

Run: `task test -- -run 'TestPropsColumn|TestRecordNoLonger|TestPropsStoresNoBase64' ./internal/ingest/`

Expected: PASS (3 tests).

- [ ] **Step 8: Run the full suite**

Run: `task test`

Expected: PASS. Tests asserting Properties survive a writer round-trip still pass via the legacy read path, which Task 4 replaces.

- [ ] **Step 9: Commit**

```bash
task fmt
git add internal/ingest/sqlite_writer.go internal/ingest/props_column_test.go
git commit -m "[mache-90b89b] feat(ingest): write Properties to a queryable props column

nodes gains props JSON, holding Properties as real nested JSON. record stops
carrying Properties, dropping from three meanings to two. json_extract on
lang/pkg/imports now works — the reason for the change."
```

______________________________________________________________________

## Task 4: Read `props`, and refuse stale mache-written `.db` files

**Files:**

- Create: `internal/graph/props_compat.go`
- Create: `internal/graph/props_compat_test.go`
- Modify: `internal/graph/nodes_table_reader.go:69-80` (constructor), `:100-145` (GetNode)
- Modify: `internal/ingest/sqlite_writer.go:605-625` (read-back)
- Modify: `internal/graph/sqlite_graph.go:150-152`
- Modify: `internal/graph/writable_graph.go:45-55`

**Interfaces:**

- Consumes: the `props` column from Task 3.

- Produces: `graph.RequireProps(db *sql.DB) error`.

- [ ] **Step 1: Write the failing test**

Create `internal/graph/props_compat_test.go`:

```go
package graph

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newNodesDB creates a nodes table with the given optional columns, mimicking
// the three producers mache must tolerate (spec §2.3).
func newNodesDB(t *testing.T, withContext, withProps bool) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	cols := "id TEXT PRIMARY KEY, parent_id TEXT, name TEXT, kind INTEGER, " +
		"size INTEGER, mtime INTEGER, record_id TEXT, record JSON, source_file TEXT"
	if withContext {
		cols += ", context BLOB"
	}
	if withProps {
		cols += ", props JSON"
	}
	_, err = db.Exec("CREATE TABLE nodes (" + cols + ")")
	require.NoError(t, err)
	return db
}

func TestRequireProps_CurrentMacheDB_OK(t *testing.T) {
	assert.NoError(t, RequireProps(newNodesDB(t, true, true)))
}

func TestRequireProps_LeylineDB_OK(t *testing.T) {
	// leyline-produced tables have neither column and never carried Properties.
	// Refusing these would break mache's primary input (leyline parse has been
	// the sole source parser since v0.18.0).
	assert.NoError(t, RequireProps(newNodesDB(t, false, false)),
		"a leyline-shaped nodes table must serve, not be refused")
}

func TestRequireProps_StaleMacheDB_Refused(t *testing.T) {
	err := RequireProps(newNodesDB(t, true, false))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mache build",
		"the error must name the remedy, not just the symptom")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `task test -- -run 'TestRequireProps' ./internal/graph/`

Expected: FAIL — `undefined: RequireProps`.

- [ ] **Step 3: Write the implementation**

Create `internal/graph/props_compat.go`:

```go
package graph

import (
	"database/sql"
	"fmt"
)

// RequireProps rejects a nodes table that mache itself wrote before the `props`
// column existed (mache-90b89b). Such a table has Properties base64'd into
// `record`, a format no current code reads.
//
// The check is scoped by producer. mache's SQLiteWriter always has `context`
// (CREATE TABLE, plus the ALTER in sqlite_writer.go); leyline-produced tables
// stop at source_file and carry no Properties at all. So `context` present with
// `props` absent is exactly "stale mache-written", and is the only case refused
// — a blanket refusal would reject every leyline .db, which since v0.18.0 is
// mache's primary input.
func RequireProps(db *sql.DB) error {
	if !ColumnExists(db, "nodes", "context") {
		return nil // leyline-produced: no Properties to lose
	}
	if ColumnExists(db, "nodes", "props") {
		return nil
	}
	return fmt.Errorf(
		"this .db predates the nodes.props column and its node properties are unreadable; rebuild it with `mache build`")
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `task test -- -run 'TestRequireProps' ./internal/graph/`

Expected: PASS (3 tests).

- [ ] **Step 5: Wire the refusal into both open paths**

`internal/graph/sqlite_graph.go`, inside `if useNodesTable {` before `NewNodesTableReader`:

```go
		if err := RequireProps(db); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("open %s: %w", dbPath, err)
		}
```

`internal/graph/writable_graph.go`, before constructing `WritableGraph`:

```go
	if err := RequireProps(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open %s: %w", masterDBPath, err)
	}
```

- [ ] **Step 6: Read props in NodesTableReader**

In the constructor at `:69-80`, add alongside `hasContext`:

```go
		hasProps: ColumnExists(db, "nodes", "props"),
```

and add `hasProps bool` to the struct.

In `GetNode`, replace the `recordExpr` block and the legacy restore (`:109-145`) with a direct `props` read. The `CASE WHEN kind = NodeKindDir` guard is no longer needed — `props` never holds file content, so there is nothing to filter:

```go
	var props []byte
	cols := "kind, size, mtime, record_id"
	if r.hasProps {
		cols += ", props"
	}
	if r.hasContext {
		cols += ", context"
	}
	dest := []any{&kind, &size, &mtimeNano, &recordID}
	if r.hasProps {
		dest = append(dest, &props)
	}
	if r.hasContext {
		dest = append(dest, &context)
	}
	err := r.db.QueryRow("SELECT "+cols+" FROM nodes WHERE id = ?", id).Scan(dest...)
```

and after the node is constructed:

```go
	if len(props) > 0 {
		var p map[string]json.RawMessage
		if json.Unmarshal(props, &p) == nil && len(p) > 0 {
			node.Properties = p
		}
	}
```

Delete the comment block at `:98-108` describing the `record`-based restore and its `CASE` performance rationale — both now describe code that no longer exists.

- [ ] **Step 7: Update the writer's read-back**

In `internal/ingest/sqlite_writer.go` `GetNode` (`:605-625`), select `props` instead of deriving Properties from `record`, and replace the `kind == graph.NodeKindDir && record.Valid` block with:

```go
	if len(props) > 0 {
		var p map[string]json.RawMessage
		if json.Unmarshal(props, &p) == nil && len(p) > 0 {
			n.Properties = p
		}
	}
```

Add `props` to that method's `SELECT` and `Scan`. This path is load-bearing: the engine re-reads a node here between the two write passes, and dropping Properties would let the second-pass `INSERT OR REPLACE` null them out (mache-d28eb1).

- [ ] **Step 8: Run the full suite**

Run: `task test`

Expected: PASS. Round-trip tests now exercise the `props` column end to end.

- [ ] **Step 9: Commit**

```bash
task fmt
git add internal/graph/props_compat.go internal/graph/props_compat_test.go \
        internal/graph/nodes_table_reader.go internal/graph/sqlite_graph.go \
        internal/graph/writable_graph.go internal/ingest/sqlite_writer.go
git commit -m "[mache-90b89b] feat(graph): read Properties from props; refuse stale mache .db files

Readers take Properties from the props column and the CASE WHEN kind guard in
NodesTableReader is deleted — props never holds file content, so there is
nothing to filter.

The cutover is hard (no legacy base64 decode path exists) but scoped by
producer: mache's writer always has `context` and leyline's never does, so
context-without-props is exactly a stale mache-written .db. A blanket refusal
would reject every leyline .db, which is mache's primary input."
```

______________________________________________________________________

## Task 5: End-to-end verification and documentation

**Files:**

- Create: `internal/graph/props_sql_test.go`
- Modify: `CHANGELOG.md`
- Modify: `docs/design/README.md`

**Interfaces:**

- Consumes: everything above.

- Produces: nothing consumed by later tasks.

- [ ] **Step 1: Write the end-to-end test**

Create `internal/graph/props_sql_test.go`. Build a real projection through the ingest engine (mirror an existing end-to-end test in `internal/graph/` for the setup), then assert against the served graph **and** the raw SQL:

```go
func TestServedNodeKeepsPropertiesAndSQLCanQueryThem(t *testing.T) {
	dbPath := buildGoFixtureDB(t) // mirror an existing e2e fixture builder

	g, err := OpenSQLiteGraph(dbPath, goSchema(t), testRenderer)
	require.NoError(t, err)
	defer g.Close()

	node, err := g.GetNode("main/functions/Handler")
	require.NoError(t, err)
	assert.Equal(t, "go", PropString(node, "lang"),
		"a construct read from .db must keep lang (the v0.19.0 serve-time fix)")

	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	require.NoError(t, err)
	defer db.Close()

	var n int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM nodes WHERE json_extract(props,'$.lang')='go'`).Scan(&n))
	assert.Positive(t, n, "SQL must be able to filter nodes by lang — the point of mache-90b89b")
}
```

- [ ] **Step 2: Run it**

Run: `task test -- -run 'TestServedNodeKeepsProperties' ./internal/graph/`

Expected: PASS.

- [ ] **Step 3: Run the full gate**

Run: `task ci`

Expected: PASS. If `task smells` reports new findings, they are real — do **not** regenerate `docs/smell-baseline.json` to silence them.

- [ ] **Step 4: Update the CHANGELOG**

Add under `## [Unreleased]`, creating the heading if absent:

```markdown
### Changed

- **Node properties moved to a dedicated `props` column** (`mache-90b89b`). They were
  base64'd into `record`, which also made that column mean three different things.
  `json_extract(props,'$.lang')` now returns `go` rather than `Z28=`, so smell rules and
  SQL consumers can filter on `lang`/`pkg`/`imports` for the first time.

### Removed

- **Reading node properties out of `record`.** `.db` files written by mache before this
  change are refused with an error naming `mache build` as the fix. Files produced by
  `leyline parse` are unaffected — they never carried properties.
```

- [ ] **Step 5: Archive the spec and plan**

Per `docs/design/README.md`, a shipped spec/plan moves to `archive/` — leaving it in `specs/`/`plans/` reads as pending work:

```bash
git mv docs/design/specs/2026-07-22-node-properties-props-column-design.md docs/design/archive/
git mv docs/design/plans/2026-07-22-node-properties-props-column.md docs/design/archive/
```

Then remove both from the **Active** list in `docs/design/README.md`.

- [ ] **Step 6: Commit and push**

```bash
task fmt
git add internal/graph/props_sql_test.go CHANGELOG.md docs/design/README.md docs/design/archive/
git commit -m "[mache-90b89b] test(graph): SQL-queryability e2e + changelog

The load-bearing assertion is the SQL one: a Go round-trip would pass even if
the stored form were still base64, since it only proves the encoder and decoder
agree with each other."
git push -u origin feat/mache-90b89b-props-column
```

- [ ] **Step 7: Open the PR and close the bead**

```bash
gh pr create --title "[mache-90b89b] feat: move node Properties to a queryable props column"
```

After merge:

```bash
rsry bead close mache-90b89b
```

______________________________________________________________________

## Self-Review

**Spec coverage:** §1 problem → Tasks 3+4. §1.1 overload → Task 3 step 6, Task 4 step 6. §2.1 schema → Task 3 steps 3-5. §2.2 type + helper → Tasks 1-2. §2.3 compat matrix → Task 4 steps 1-5. §2.4 non-goals → nothing implements them, as intended. §3 tests 1-6 → Task 3 steps 1 (tests 1, 5, 6), Task 4 step 1 (tests 3, 4), Task 5 step 1 (test 2 + queryability). §4 consequences → Task 5 step 4.

**Naming consistency:** `PropString`/`SetPropString`/`PropRaw`/`SetPropRaw`/`RequireProps` and the `hasProps` field are used identically in every task.

**Known risk:** Tasks 3 and 5 reference fixture builders (`NewSQLiteWriter` construction, `buildGoFixtureDB`, `goSchema`, `testRenderer`) whose exact signatures were not read while writing this plan. Both steps say to mirror an existing test in the same package rather than invent one. This is the one place the implementer must read surrounding code instead of copying the plan verbatim.
