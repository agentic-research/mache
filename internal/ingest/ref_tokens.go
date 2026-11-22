package ingest

import "strings"

// refTokens is the reference tokens one rendered `refs` template names. A
// template that renders a single line names one token, exactly as rendered.
// One that renders several lines names one token per non-blank line, trimmed,
// so a list-valued field is one template:
//
//	"refs": ["{{range .imports}}{{.}}\n{{end}}"]
//
// Without this a record's list of refs needed one guarded template per
// possible index (#820). A newline could never be part of a token another
// node defines, so no working single-token ref changes meaning.
func refTokens(rendered string) []string {
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
