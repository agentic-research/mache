package sqlcount

import (
	"database/sql"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"

	"modernc.org/sqlite/vfs"
)

// Statement counts catch a lookup that turns into a LOOP of statements. They
// cannot see one statement that does a whole-table's work: replacing an
// anchored walk with a query on LLO's v_node_path is still one statement, and
// it reads every node in the database (mache-90cb9f). What does see it is how
// much of the FILE a query reads. An indexed lookup reads a few pages per
// b-tree level, so its bytes grow with log(n); a scan reads the table, so its
// bytes grow with n. Like a statement count, that is a property of the plan,
// not of the machine running it.
//
// The bytes are counted below SQLite, through modernc's read-only VFS over an
// fs.FS that tallies every Read. Nothing in the query or the reader under test
// changes to be measured.

// ReadCounted is a read-only database whose file reads are counted.
type ReadCounted struct {
	DB    *sql.DB
	fsvfs *vfs.FS
	bytes *atomic.Int64
}

// OpenReadCounted opens the database at path read-only, counting the bytes
// SQLite reads from its file. The page cache is per connection, so the count
// for a query includes the pages it had to fetch, not ones already cached by
// an earlier query on the same connection.
func OpenReadCounted(path string) (*ReadCounted, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	n := new(atomic.Int64)
	name, fsvfs, err := vfs.New(countingFS{fs: os.DirFS(filepath.Dir(abs)), n: n})
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Base(abs)+"?vfs="+name)
	if err != nil {
		_ = fsvfs.Close()
		return nil, err
	}
	// One connection, so every query shares one page cache and the count is
	// the work of the queries asked, not of connections the pool opened.
	db.SetMaxOpenConns(1)
	return &ReadCounted{DB: db, fsvfs: fsvfs, bytes: n}, nil
}

// Reset zeroes the byte counter and returns a func reading it.
func (r *ReadCounted) Reset() (bytes func() int64) {
	r.bytes.Store(0)
	return r.bytes.Load
}

// Close closes the database, then unregisters its VFS.
func (r *ReadCounted) Close() error {
	if err := r.DB.Close(); err != nil {
		return err
	}
	return r.fsvfs.Close()
}

type countingFS struct {
	fs fs.FS
	n  *atomic.Int64
}

func (c countingFS) Open(name string) (fs.File, error) {
	f, err := c.fs.Open(name)
	if err != nil {
		return nil, err
	}
	return &countingFile{File: f, n: c.n}, nil
}

// countingFile counts what it reads and passes Seek through, which the VFS
// needs to read a page at its offset.
type countingFile struct {
	fs.File
	n *atomic.Int64
}

func (f *countingFile) Read(p []byte) (int, error) {
	k, err := f.File.Read(p)
	f.n.Add(int64(k))
	return k, err
}

func (f *countingFile) Seek(offset int64, whence int) (int64, error) {
	return f.File.(io.Seeker).Seek(offset, whence)
}
