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
)

// RefsModule implements vtab.Module. It is a process-wide singleton because
// modernc.org/sqlite registers modules globally (driver-level, not per-DB).
// The mutable refsDB pointer is updated each time OpenSQLiteGraph runs.
type RefsModule struct {
	mu     sync.RWMutex
	refsDB *sql.DB
}

// Register registers the mache_refs module with the global SQLite driver.
// Safe to call multiple times — only the first call registers. Returns the
// singleton so callers can set the refsDB pointer via SetRefsDB.
func Register() *RefsModule {
	once.Do(func() {
		singleton = &RefsModule{}
		// db parameter is unused by the engine; pass nil.
		if err := vtab.RegisterModule(nil, "mache_refs", singleton); err != nil {
			panic(fmt.Sprintf("refsvtab: register module: %v", err))
		}
	})
	return singleton
}

// SetRefsDB updates the sidecar database pointer. Must be called after
// opening the refsDB connection and before any queries hit the vtab.
func (m *RefsModule) SetRefsDB(db *sql.DB) {
	m.mu.Lock()
	m.refsDB = db
	m.mu.Unlock()
}

// getRefsDB returns the current refsDB pointer under read lock.
func (m *RefsModule) getRefsDB() *sql.DB {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.refsDB
}

// ---------------------------------------------------------------------------
// vtab.Module
// ---------------------------------------------------------------------------

func (m *RefsModule) Create(ctx vtab.Context, args []string) (vtab.Table, error) {
	if err := ctx.Declare("CREATE TABLE x(token TEXT, path TEXT)"); err != nil {
		return nil, err
	}
	return &refsTable{mod: m}, nil
}

func (m *RefsModule) Connect(ctx vtab.Context, args []string) (vtab.Table, error) {
	return m.Create(ctx, args)
}

// ---------------------------------------------------------------------------
// vtab.Table
// ---------------------------------------------------------------------------

type refsTable struct {
	mod *RefsModule
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
	return &refsCursor{mod: t.mod}, nil
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
	mod  *RefsModule
	rows []refsRow
	pos  int
}

func (c *refsCursor) Filter(idxNum int, idxStr string, vals []vtab.Value) error {
	c.rows = c.rows[:0]
	c.pos = 0

	db := c.mod.getRefsDB()
	if db == nil {
		return nil // no refsDB yet — return empty
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
			continue
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("refsvtab: filtered scan rows: %w", err)
	}
	_ = rows.Close()

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
			continue
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("refsvtab: scan node_refs rows: %w", err)
	}
	_ = rows.Close()

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
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			continue
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
