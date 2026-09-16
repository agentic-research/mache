package build

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/buildinfo"
	internalingest "github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/leyline"
)

// fingerprintKey is the _mache_meta row naming what produced a projection.
// A build may only reuse an output db whose fingerprint matches exactly.
const fingerprintKey = "projection_fingerprint"

// projectionFingerprint identifies everything that decides what a projection
// LOOKS like, so a reused db can never be one an older or differently
// configured build wrote.
//
// Three inputs, each load-bearing:
//
//   - the schema, because building the same source with a different topology
//     into the same path must not merge two projections together;
//   - the mache version, because reusing across versions is old projection
//     logic serving under a new binary — the failure mache-6c9e1d records,
//     which mount already keys against;
//   - the pinned leyline version, because a different parser writes a
//     different `_ast` and the projection is derived from it.
//
// A mismatch is not an error. It just means a full rebuild, which is what
// every build did before this.
func projectionFingerprint(topology *api.Topology) (string, error) {
	canonical, err := json.Marshal(topology)
	if err != nil {
		return "", fmt.Errorf("fingerprint schema: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return fmt.Sprintf("schema=%s mache=%s leyline=%s",
		hex.EncodeToString(sum[:8]), buildinfo.Version, leyline.BinaryVersion), nil
}

// reusableIndex returns the file index of an existing projection at output
// when that projection was written by an identical build, and nil otherwise.
//
// nil means "project everything", which is what this path always did. The
// caller removes the file in that case, because a partial merge into a
// projection written by something else is the one outcome worse than a slow
// build.
func reusableIndex(output, want string) map[string]internalingest.FileIndexEntry {
	if _, err := os.Stat(output); err != nil {
		return nil
	}
	if got, err := readFingerprint(output); err != nil || got != want {
		return nil
	}
	index, err := internalingest.LoadFileIndex(output)
	if err != nil || len(index) == 0 {
		return nil
	}
	return index
}

func readFingerprint(output string) (string, error) {
	db, err := sql.Open("sqlite", output+"?mode=ro")
	if err != nil {
		return "", err
	}
	defer func() { _ = db.Close() }()

	var value string
	err = db.QueryRow(`SELECT value FROM _mache_meta WHERE key = ?`, fingerprintKey).Scan(&value)
	if err != nil {
		return "", err
	}
	return value, nil
}

// writeFingerprint stamps what produced this projection, so the next build can
// tell whether reusing it is safe.
func writeFingerprint(output, fingerprint string) error {
	db, err := sql.Open("sqlite", output)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS _mache_meta (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`); err != nil {
		return err
	}
	_, err = db.Exec(`INSERT OR REPLACE INTO _mache_meta (key, value) VALUES (?, ?)`,
		fingerprintKey, fingerprint)
	return err
}
