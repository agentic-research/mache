package template

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A field the data does not have prints as nothing, never as Go's
// `<no value>` placeholder, which is text no record held (mache-b33274).
// Everything else prints and behaves exactly as before.
func TestRender_AMissingFieldPrintsNothing(t *testing.T) {
	vals := map[string]any{
		"present": "P", "zero": 0, "f": false,
		"list":   []any{"a", "b"},
		"nested": map[string]any{"k": "v"},
		"empty":  "",
	}
	for tmpl, want := range map[string]string{
		// Missing, at any depth, through a variable: empty.
		"[{{.missing}}]":                   "[]",
		"[{{.nested.absent}}]":             "[]",
		"[{{.missing.deep}}]":              "[]",
		"[{{$x := .missing}}{{$x}}]":       "[]",
		"a {{.missing}} b {{.present}}":    "a  b P",
		"{{if true}}[{{.missing}}]{{end}}": "[]",
		// Present values, including the zero ones, as before.
		"[{{.present}}]":  "[P]",
		"[{{.zero}}]":     "[0]",
		"[{{.f}}]":        "[false]",
		"[{{.empty}}]":    "[]",
		"[{{.list}}]":     "[[a b]]",
		"[{{.nested.k}}]": "[v]",
		// Control structures still see a missing field as missing.
		`[{{default .missing "dflt"}}]`:                   "[dflt]",
		"[{{if .missing}}yes{{else}}no{{end}}]":           "[no]",
		"[{{range .missing}}x{{end}}]":                    "[]",
		"[{{range .list}}{{.}},{{end}}]":                  "[a,b,]",
		"[{{with .missing}}w{{else}}none{{end}}]":         "[none]",
		"[{{range .missing}}x{{else}}{{.absent}}{{end}}]": "[]",
	} {
		got, err := Render(tmpl, vals)
		require.NoError(t, err, tmpl)
		assert.Equal(t, want, got, tmpl)
		assert.NotContains(t, got, "<no value>", tmpl)

		cached, err := RenderWithFuncs(tmpl, vals, nil, &sync.Map{})
		require.NoError(t, err, tmpl)
		assert.Equal(t, want, cached, "RenderWithFuncs: %s", tmpl)
	}
}
