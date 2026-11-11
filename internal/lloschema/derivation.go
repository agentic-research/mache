package lloschema

import (
	"errors"
	"fmt"
	"strings"
)

// ErrNoParserID is returned for a database that records no parser identity:
// one leyline wrote before projection-v11, or one mache wrote.
var ErrNoParserID = errors.New("the database records no parserId (_meta.derived_from v2, leyline >= v0.26.0)")

// ParserID returns the parserId the database's projection was derived under:
// the third field of `_meta.derived_from`, "v2 <storeRoot> <parserId>
// <projection>" (LLO ADR-0040 D6, ADR-0047 D-ii). It folds the grammar
// versions, the IR schema version and the extraction and query-set epochs, so
// two parses with the same parserId derive the same rows from the same bytes,
// and any change to how rows are derived moves it (mache-3465b9). A leyline
// release moves it, and so does LLO_EXTRACTION_EPOCH.
func ParserID(qg Querier) (string, error) {
	rows, err := qg.QueryRefs(`SELECT value FROM _meta WHERE key = 'derived_from'`)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoParserID, err)
	}
	defer func() { _ = rows.Close() }()
	var v string
	if rows.Next() {
		if err := rows.Scan(&v); err != nil {
			return "", err
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	f := strings.Fields(v)
	if len(f) != 4 || f[0] != "v2" || f[2] == "" {
		return "", fmt.Errorf("%w: derived_from is %q", ErrNoParserID, v)
	}
	return f[2], nil
}
