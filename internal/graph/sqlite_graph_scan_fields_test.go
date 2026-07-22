package graph

import (
	"reflect"
	"testing"
)

// TestExtractFieldPaths pins the behavior of the name-template field extractor,
// which decides which `json_extract($.path)` columns the scan query selects.
//
// It replaced a `\.(\w+(?:\.\w+)*)` regex over the raw template string with a
// real text/template parse + FieldNode walk. The real-template cases below must
// be identical under both; the "literal dot" case is where they intentionally
// DIVERGE — see TestExtractFieldPaths_LiteralDotsAreNotFields.
func TestExtractFieldPaths(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []string
		want []string
	}{
		{"simple field", []string{"{{.pkg}}"}, []string{"pkg"}},
		{"nested path", []string{"{{.item.cve.id}}"}, []string{"item.cve.id"}},
		{
			"func arg (real nvd-schema shape)",
			[]string{"{{slice .item.cve.published 0 4}}"},
			[]string{"item.cve.published"},
		},
		{
			"dedup + sort across templates",
			[]string{"{{.item.cve.id}}", "{{slice .item.cve.published 5 7}}", "{{.item.cve.id}}"},
			[]string{"item.cve.id", "item.cve.published"},
		},
		{"static template — no fields", []string{"functions"}, []string{}},
		{"empty input", nil, []string{}},
		{
			"multiple fields in one action",
			[]string{`{{if .a.b}}{{.c.d}}{{end}}`},
			[]string{"a.b", "c.d"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := extractFieldPaths(tc.in)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("extractFieldPaths(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestExtractFieldPaths_LiteralDotsAreNotFields documents the bug the
// structural parse fixes. The old regex scanned the WHOLE template string, so a
// dot in literal text (outside any {{action}}) was captured as a field path —
// yielding a phantom `json_extract(record, '$.txt')` column that always
// resolves NULL. A template parser only sees real field references.
func TestExtractFieldPaths_LiteralDotsAreNotFields(t *testing.T) {
	got := extractFieldPaths([]string{"report.txt {{.id}}"})
	want := []string{"id"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("extractFieldPaths = %v, want %v — literal 'report.txt' must NOT "+
			"become a field path (the old regex captured 'txt')", got, want)
	}
}

// TestExtractFieldPaths_UnparseableTemplateIsSkipped ensures a malformed
// template can't panic or poison the column list. Such a template would fail at
// render time anyway, so contributing no field paths is the safe behavior.
func TestExtractFieldPaths_UnparseableTemplateIsSkipped(t *testing.T) {
	got := extractFieldPaths([]string{"{{.unclosed", "{{.good}}"})
	want := []string{"good"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("extractFieldPaths = %v, want %v", got, want)
	}
}
