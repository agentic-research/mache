package refsvtab

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"

	"github.com/RoaringBitmap/roaring"
	"modernc.org/sqlite/vtab"
)

// singleton holds the one RefsModule registered with the SQLite driver.
var (
	once      sync.Once
	singleton *RefsModule
	initErr   error
)

// RefsModule implements vtab.Module. It is a process-wide singleton because
// modernc.org/sqlite registers modules globally (driver-level, not per-DB).
type RefsModule struct {
	mu sync.RWMutex
	// dbs maps a unique ID (passed as argument to CREATE VIRTUAL TABLE)
	// to the *sql.DB instance containing the sidecar tables.
	dbs map[string]*sql.DB
}

// Register registers the mache_refs module with the global SQLite driver.
// Safe to call multiple times — only the first call registers. Returns the
// singleton so callers can register their DB instances via RegisterDB.
func Register() (*RefsModule, error) {
	once.Do(func() {
		singleton = &RefsModule{
			dbs: make(map[string]*sql.DB),
		}
		// db parameter is unused by the engine; pass nil.
		if err := vtab.RegisterModule(nil, "mache_refs", singleton); err != nil {
			initErr = fmt.Errorf("refsvtab: register module: %w", err)
			singleton = nil
		}
	})
	return singleton, initErr
}

// RegisterDB registers a database connection with a unique ID.
// The ID must be passed to CREATE VIRTUAL TABLE ... USING mache_refs(id).
func (m *RefsModule) RegisterDB(id string, db *sql.DB) {
	m.mu.Lock()
	m.dbs[id] = db
	m.mu.Unlock()
}

// UnregisterDB removes a database connection from the registry.
// Should be called when the graph is closed.
func (m *RefsModule) UnregisterDB(id string) {
	m.mu.Lock()
	delete(m.dbs, id)
	m.mu.Unlock()
}

// ---------------------------------------------------------------------------
// vtab.Module
// ---------------------------------------------------------------------------

func (m *RefsModule) Create(ctx vtab.Context, args []string) (vtab.Table, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("mache_refs: missing DB ID argument")
	}
	// args[0] is module name, args[1] is table name, args[2]... are arguments.
	// But modernc.org/sqlite passes arguments differently?
	// According to docs/source, args includes module name etc?
	// Actually, standard SQLite xCreate(db, pAux, argc, argv).
	// modernc adapter passes args as []string.
	// argv[0] = module name
	// argv[1] = database name
	// argv[2] = table name
	// argv[3]... = arguments inside ()
	//
	// So if we do USING mache_refs(my_id), args[3] should be "my_id".
	if len(args) < 4 {
		return nil, fmt.Errorf("mache_refs: missing DB ID argument (expected USING mache_refs(id))")
	}
	id := args[3]

	m.mu.RLock()
	db, ok := m.dbs[id]
	m.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("mache_refs: unknown DB ID %q", id)
	}

	if err := ctx.Declare("CREATE TABLE x(token TEXT, path TEXT)"); err != nil {
		return nil, err
	}
	return &refsTable{mod: m, db: db}, nil
}

func (m *RefsModule) Connect(ctx vtab.Context, args []string) (vtab.Table, error) {
	return m.Create(ctx, args)
}

// ---------------------------------------------------------------------------
// vtab.Table
// ---------------------------------------------------------------------------

type refsTable struct {
	mod *RefsModule
	db  *sql.DB
}

func (t *refsTable) BestIndex(info *vtab.IndexInfo) error {
	for i := range info.Constraints {
		c := &info.Constraints[i]
		if !c.Usable || c.Column != 0 {
			continue
		}
		switch c.Op {
		case vtab.OpEQ:
			// Token equality lookup — cheap.
			c.ArgIndex = 0
			c.Omit = true
			info.IdxNum = 1
			info.EstimatedCost = 1
			info.EstimatedRows = 10
			return nil
		case vtab.OpLIKE:
			// LIKE pattern — SQLite can use PK index for prefix patterns.
			c.ArgIndex = 0
			c.Omit = true
			info.IdxNum = 2
			info.EstimatedCost = 100
			info.EstimatedRows = 100
			return nil
		case vtab.OpGLOB:
			// GLOB pattern — same as LIKE but case-sensitive.
			c.ArgIndex = 0
			c.Omit = true
			info.IdxNum = 3
			info.EstimatedCost = 100
			info.EstimatedRows = 100
			return nil
		}
	}
	// Full scan — expensive.
	info.IdxNum = 0
	info.EstimatedCost = 1e6
	info.EstimatedRows = 1e6
	return nil
}

func (t *refsTable) Open() (vtab.Cursor, error) {
	return &refsCursor{table: t}, nil
}

func (t *refsTable) Disconnect() error { return nil }
func (t *refsTable) Destroy() error    { return nil }

// ---------------------------------------------------------------------------
// vtab.Cursor
// ---------------------------------------------------------------------------

type refsRow struct {
	token string
	path  string
}

type refsCursor struct {
	table *refsTable
	rows  []refsRow
	pos   int
}

func (c *refsCursor) Filter(idxNum int, idxStr string, vals []vtab.Value) error {
	c.rows = c.rows[:0]
	c.pos = 0

	db := c.table.db
	if db == nil {
		return nil // paranoid check
	}

	switch idxNum {
	case 1:
		// Token equality lookup.
		token, ok := vals[0].(string)
		if !ok {
			return nil
		}
		return c.loadToken(db, token)
	case 2:
		// LIKE pattern.
		pattern, ok := vals[0].(string)
		if !ok {
			return nil
		}
		return c.loadFiltered(db, "LIKE", pattern)
	case 3:
		// GLOB pattern.
		pattern, ok := vals[0].(string)
		if !ok {
			return nil
		}
		return c.loadFiltered(db, "GLOB", pattern)
	default:
		// Full scan: iterate all tokens.
		return c.loadAll(db)
	}
}

// loadToken resolves a single token's bitmap into (token, path) rows.
func (c *refsCursor) loadToken(db *sql.DB, token string) error {
	var blob []byte
	err := db.QueryRow("SELECT bitmap FROM node_refs WHERE token = ?", token).Scan(&blob)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("refsvtab: query token %q: %w", token, err)
	}

	return c.expandBitmap(db, token, blob)
}

// loadFiltered queries tokens matching a LIKE or GLOB pattern and expands
// their bitmaps. Same materialization pattern as loadAll — collect rows first,
// close cursor, then expand. SQLite can use the PRIMARY KEY index for prefix
// patterns (e.g., "Login%").
func (c *refsCursor) loadFiltered(db *sql.DB, op, pattern string) error {
	type entry struct {
		token string
		blob  []byte
	}

	query := fmt.Sprintf("SELECT token, bitmap FROM node_refs WHERE token %s ?", op)
	rows, err := db.Query(query, pattern)
	if err != nil {
		return fmt.Errorf("refsvtab: filtered scan (%s %q): %w", op, pattern, err)
	}

	var entries []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.token, &e.blob); err != nil {
			continue // scan failure on a single row; remaining rows may still be valid
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close() // safe to ignore
		return fmt.Errorf("refsvtab: filtered scan rows: %w", err)
	}
	_ = rows.Close() // safe to ignore

	for _, e := range entries {
		if err := c.expandBitmap(db, e.token, e.blob); err != nil {
			return err
		}
	}
	return nil
}

// loadAll iterates every row in node_refs and expands all bitmaps.
// Materializes all (token, blob) pairs first, then closes the cursor before
// calling expandBitmap — which itself needs a database connection. With
// MaxOpenConns(2), the outer vtab query holds conn 1, so both the scan here
// and expandBitmap share conn 2 sequentially.
func (c *refsCursor) loadAll(db *sql.DB) error {
	type entry struct {
		token string
		blob  []byte
	}

	rows, err := db.Query("SELECT token, bitmap FROM node_refs")
	if err != nil {
		return fmt.Errorf("refsvtab: scan node_refs: %w", err)
	}

	var entries []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.token, &e.blob); err != nil {
			continue // scan failure on a single row; remaining rows may still be valid
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close() // safe to ignore
		return fmt.Errorf("refsvtab: scan node_refs rows: %w", err)
	}
	_ = rows.Close() // safe to ignore

	for _, e := range entries {
		if err := c.expandBitmap(db, e.token, e.blob); err != nil {
			return err
		}
	}
	return nil
}

// expandBitmap deserializes a roaring bitmap and resolves file IDs to paths.
func (c *refsCursor) expandBitmap(db *sql.DB, token string, blob []byte) error {
	rb := roaring.New()
	if err := rb.UnmarshalBinary(blob); err != nil {
		return fmt.Errorf("refsvtab: unmarshal bitmap for %q: %w", token, err)
	}

	var fileIDs []uint32
	it := rb.Iterator()
	for it.HasNext() {
		fileIDs = append(fileIDs, it.Next())
	}
	if len(fileIDs) == 0 {
		return nil
	}

	// Resolve file IDs → paths.
	args := make([]any, len(fileIDs))
	placeholders := make([]string, len(fileIDs))
	for i, id := range fileIDs {
		args[i] = id
		placeholders[i] = "?"
	}

	query := fmt.Sprintf("SELECT path FROM file_ids WHERE id IN (%s)", strings.Join(placeholders, ","))
	rows, err := db.Query(query, args...)
	if err != nil {
		return fmt.Errorf("refsvtab: resolve file_ids: %w", err)
	}
	defer func() { _ = rows.Close() }() // safe to ignore

	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			continue // scan failure on a single row; remaining rows may still be valid
		}
		c.rows = append(c.rows, refsRow{token: token, path: path})
	}
	return rows.Err()
}

func (c *refsCursor) Next() error {
	c.pos++
	return nil
}

func (c *refsCursor) Eof() bool {
	return c.pos >= len(c.rows)
}

func (c *refsCursor) Column(col int) (vtab.Value, error) {
	if c.pos >= len(c.rows) {
		return nil, nil
	}
	switch col {
	case 0:
		return c.rows[c.pos].token, nil
	case 1:
		return c.rows[c.pos].path, nil
	default:
		return nil, nil
	}
}

func (c *refsCursor) Rowid() (int64, error) {
	return int64(c.pos), nil
}

func (c *refsCursor) Close() error {
	c.rows = nil
	return nil
}
