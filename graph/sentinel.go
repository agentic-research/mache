package graph

import "strings"

// FileLevelSentinelPrefix marks a node_refs row that records a FILE-level
// reference rather than a reference from a construct.
//
// The engine emits these for patterns like cobra's `RunE: handler`, where a
// token is referenced at file scope with no enclosing function. dead_code needs
// them: its alive-set check is token-only, and without the sentinel every
// handler assigned that way looks dead. Attributing the ref to some arbitrary
// function in the file instead would be worse — it made every function in a
// cobra file appear to call the callback (mache-02r9).
//
// So the row is correct. What it is NOT is user content: `_file_level:` ids
// name no construct anyone can open, refactor, or reason about, and they carry
// an ABSOLUTE path, so they are not even portable. They are bookkeeping, and
// they belong to the producer side of the schema.
const FileLevelSentinelPrefix = "_file_level:"

// sentinelSQLPattern is the LIKE form of the same predicate. Derived from the
// prefix rather than typed out, because six separate call sites had
// hand-written the literal and any one of them could drift from the others.
const sentinelSQLPattern = FileLevelSentinelPrefix + "%"

// IsFileLevelSentinel reports whether a node id is engine bookkeeping rather
// than a construct a consumer can act on.
func IsFileLevelSentinel(nodeID string) bool {
	return strings.HasPrefix(nodeID, FileLevelSentinelPrefix)
}

// filterSentinelRefs returns refs with every sentinel node id removed, and with
// tokens left empty by that removal dropped entirely.
//
// Dropping the emptied tokens matters: callers count len(refs) as "reference
// tokens" and feed the keys to community detection, so a token whose only refs
// were bookkeeping would otherwise survive as a real-looking node with no
// edges. Returns a new map; the input is not mutated, because MemoryStore hands
// out a memoized snapshot that callers must treat as read-only.
func filterSentinelRefs(refs map[string][]string) map[string][]string {
	out := make(map[string][]string, len(refs))
	for token, ids := range refs {
		kept := make([]string, 0, len(ids))
		for _, id := range ids {
			if !IsFileLevelSentinel(id) {
				kept = append(kept, id)
			}
		}
		if len(kept) > 0 {
			out[token] = kept
		}
	}
	return out
}
