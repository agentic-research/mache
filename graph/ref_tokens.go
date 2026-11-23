package graph

import "strings"

// RefTokens is the reference tokens one rendered `refs` template names. A
// template that renders a single line names one token, exactly as rendered.
// One that renders several lines names one token per non-blank line, trimmed,
// so a list-valued field is one template:
//
//	"refs": ["{{range .imports}}{{.}}\n{{end}}"]
//
// Without this a record's list of refs needed one guarded template per
// possible index (#820). A newline could never be part of a token another
// node defines, so no working single-token ref changes meaning. It is the one
// rule for every path that renders refs: the ingest engine and the direct
// scan.
func RefTokens(rendered string) []string {
	if rendered == "" {
		return nil
	}
	if !strings.Contains(rendered, "\n") {
		return []string{rendered}
	}
	var tokens []string
	for line := range strings.SplitSeq(rendered, "\n") {
		if tok := strings.TrimSpace(line); tok != "" {
			tokens = append(tokens, tok)
		}
	}
	return tokens
}

// DefTokensForName is the tokens a construct named name is defined under on a
// data projection: the name, and for a dotted name its last segment, so a
// reference to `Method` reaches `Receiver.Method` and one to `EulerSum`
// reaches `Theory.EulerSum`. The ingest engine registers the same two
// (processNode); the direct scan uses this, and a parity test holds them
// together.
func DefTokensForName(name string) []string {
	if dot := strings.LastIndex(name, "."); dot > 0 && dot < len(name)-1 {
		return []string{name, name[dot+1:]}
	}
	return []string{name}
}

// SegmentEscape makes a rendered name usable as exactly ONE segment of a node
// ID. A node ID is a '/'-joined path and every consumer splits it back on '/':
// SQLiteWriter.AddNode derives parent_id and name from the LAST '/', GraphFS
// walks it component by component, ListChildren is `WHERE parent_id = ?`. A
// name template is free to render a '/' — every non-stdlib Go import path,
// every relative JS import — and the ID then split in the wrong place:
// `main/imports/"golden/store"` was written with parent_id `main/imports/"golden`,
// which is not a node, so ListChildren never returned it and 1939 nodes were
// unreachable on mache itself (mache-94f571).
//
// Percent-encoding, '%' FIRST so the mapping stays injective: escaping only
// '/' would give the distinct names `a/b` and `a%2Fb` the same ID.
//
// Only the ID is escaped. The rendered name is still what AddDef registers as
// the definition token, so `search` and the smell rules keep matching the
// symbol as it is written in the source.
//
// The engine (internal/ingest) and the direct scan both escape through this,
// so a name holds the same ID on both paths. The direct scan used to join the
// raw name, so `io/fs` became `mods/a/io/fs` under `mods/a`, a node nothing
// could reach (mache-b33274).
func SegmentEscape(name string) string {
	if !strings.ContainsAny(name, "%/") {
		return name
	}
	name = strings.ReplaceAll(name, "%", "%25")
	return strings.ReplaceAll(name, "/", "%2F")
}

// UnnamedName is the directory a record gets when its name renders empty,
// usually because the record lacks the field the name is made of.
//
// The record is kept, not dropped: its files and its references are still
// data. But it has no name, so it defines no symbol: find_definition does
// not find it, and nothing that references a name reaches it. A second
// unnamed record under the same parent is `_unnamed.2`, by the same rule as
// any other repeated name, on both paths and in record order.
//
// It used to be one of two wrong things: the engine named the directory
// `<no value>`, Go's placeholder for a missing field, which also defined
// `<no value>` as a symbol shared by every such record; the direct path
// dropped the record (mache-b33274). An empty name is never used as is: it
// would give the directory its parent's ID.
const UnnamedName = "_unnamed"

// NameOrUnnamed is the directory name for a rendered name: the name itself,
// or UnnamedName when it rendered empty, with unnamed saying which.
func NameOrUnnamed(rendered string) (name string, unnamed bool) {
	if rendered == "" {
		return UnnamedName, true
	}
	return rendered, false
}
