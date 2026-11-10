package fixturedb

import "database/sql"

// TryCommit commits one write to the db at path on a connection of its own,
// without waiting for a lock, and returns the error: SQLITE_BUSY while some
// other connection holds a lock on the file. A test observes from outside
// whether a reader keeps its lock between statements (mache-86c666).
func TryCommit(path string) error {
	w, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(0)")
	if err != nil {
		return err
	}
	defer func() { _ = w.Close() }()
	_, err = w.Exec(`CREATE TABLE IF NOT EXISTS zz_writer_probe (x)`)
	return err
}
