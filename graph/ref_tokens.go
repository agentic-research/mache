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
