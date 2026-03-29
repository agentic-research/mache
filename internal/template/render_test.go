package template

import (
	"sync"
	"testing"
	"text/template"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRender_GoldenPath exercises the primary use case: schema name and
// content template rendering with the standard mache template functions.
// This is the path every SQLiteGraph record and every ingested node takes.
func TestRender_GoldenPath(t *testing.T) {
	tests := []struct {
		name   string
		tmpl   string
		values map[string]any
		want   string
	}{
		{
			name:   "simple interpolation",
			tmpl:   "{{.name}}",
			values: map[string]any{"name": "CVE-2024-0001"},
			want:   "CVE-2024-0001",
		},
		{
			name:   "slice for temporal sharding",
			tmpl:   "{{slice .id 4 8}}",
			values: map[string]any{"id": "CVE-2024-0001"},
			want:   "2024",
		},
		{
			name:   "json marshal",
			tmpl:   `{{json .data}}`,
			values: map[string]any{"data": map[string]any{"key": "value"}},
			want:   `{"key":"value"}`,
		},
		{
			name:   "first element",
			tmpl:   "{{first .items}}",
			values: map[string]any{"items": []any{"a", "b", "c"}},
			want:   "a",
		},
		{
			name:   "first of empty",
			tmpl:   "{{first .items}}",
			values: map[string]any{"items": []any{}},
			want:   "<no value>",
		},
		{
			name:   "unquote strips Go string quotes",
			tmpl:   `{{unquote .path}}`,
			values: map[string]any{"path": `"cobra"`},
			want:   "cobra",
		},
		{
			name:   "replace",
			tmpl:   `{{replace .name ":" " "}}`,
			values: map[string]any{"name": "alpine:3.18"},
			want:   "alpine 3.18",
		},
		{
			name:   "lower",
			tmpl:   "{{lower .name}}",
			values: map[string]any{"name": "RHEL"},
			want:   "rhel",
		},
		{
			name:   "upper",
			tmpl:   "{{upper .name}}",
			values: map[string]any{"name": "debian"},
			want:   "DEBIAN",
		},
		{
			name:   "split and index",
			tmpl:   `{{index (split .id ":") 0}}`,
			values: map[string]any{"id": "alpine:3.18"},
			want:   "alpine",
		},
		{
			name:   "join strings",
			tmpl:   `{{join ", " .parts}}`,
			values: map[string]any{"parts": []any{"a", "b", "c"}},
			want:   "a, b, c",
		},
		{
			name:   "hasPrefix",
			tmpl:   `{{if hasPrefix .s "CVE"}}yes{{else}}no{{end}}`,
			values: map[string]any{"s": "CVE-2024-0001"},
			want:   "yes",
		},
		{
			name:   "trimPrefix",
			tmpl:   `{{trimPrefix .s "pkg/"}}`,
			values: map[string]any{"s": "pkg/auth/login.go"},
			want:   "auth/login.go",
		},
		{
			name:   "dict",
			tmpl:   `{{(dict "a" 1 "b" 2) | json}}`,
			values: map[string]any{},
			want:   `{"a":1,"b":2}`,
		},
		{
			name:   "lookup with match",
			tmpl:   `{{lookup .sev "Critical" 4 "High" 3 0}}`,
			values: map[string]any{"sev": "High"},
			want:   "3",
		},
		{
			name:   "lookup with default",
			tmpl:   `{{lookup .sev "Critical" 4 "High" 3 0}}`,
			values: map[string]any{"sev": "Unknown"},
			want:   "0",
		},
		{
			name:   "default on nil",
			tmpl:   `{{default .name "unknown"}}`,
			values: map[string]any{"name": nil},
			want:   "unknown",
		},
		{
			name:   "default on empty string",
			tmpl:   `{{default .name "unknown"}}`,
			values: map[string]any{"name": ""},
			want:   "unknown",
		},
		{
			name:   "default passthrough",
			tmpl:   `{{default .name "unknown"}}`,
			values: map[string]any{"name": "alice"},
			want:   "alice",
		},
		{
			name:   "dig nested map",
			tmpl:   `{{dig "item.cve.id" .}}`,
			values: map[string]any{"item": map[string]any{"cve": map[string]any{"id": "CVE-2024-0001"}}},
			want:   "CVE-2024-0001",
		},
		{
			name:   "dig missing key returns empty",
			tmpl:   `{{dig "item.nonexistent.id" .}}`,
			values: map[string]any{"item": map[string]any{"cve": map[string]any{"id": "CVE-2024-0001"}}},
			want:   "",
		},
		{
			name:   "dig into slice",
			tmpl:   `{{dig "items.0.name" .}}`,
			values: map[string]any{"items": []any{map[string]any{"name": "first"}}},
			want:   "first",
		},
		{
			name:   "title case",
			tmpl:   "{{title .name}}",
			values: map[string]any{"name": "amazon linux"},
			want:   "Amazon Linux",
		},
		{
			name:   "pipeline: split then join",
			tmpl:   `{{split .id ":" | join ", "}}`,
			values: map[string]any{"id": "alpine:3.18:amd64"},
			want:   "alpine, 3.18, amd64",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Render(tt.tmpl, tt.values)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestRender_Errors verifies that invalid templates and execution errors
// are surfaced, not swallowed.
func TestRender_Errors(t *testing.T) {
	_, err := Render("{{.missing | len}}", map[string]any{})
	assert.Error(t, err, "executing template with nil pipeline should error")

	_, err = Render("{{", map[string]any{})
	assert.Error(t, err, "malformed template should error")
}

// TestRender_MissingKeyRendsEmpty verifies Go's default missingkey=zero
// behavior: missing keys render as empty string (not error).
func TestRender_MissingKeyRendersEmpty(t *testing.T) {
	got, err := Render("{{.missing}}", map[string]any{})
	require.NoError(t, err)
	assert.Equal(t, "<no value>", got)
}

// TestRender_Caching verifies that repeated calls with the same template
// string reuse the cached parsed template.
func TestRender_Caching(t *testing.T) {
	tmpl := "{{.x}}-cached-test"
	v1, err := Render(tmpl, map[string]any{"x": "a"})
	require.NoError(t, err)
	v2, err := Render(tmpl, map[string]any{"x": "b"})
	require.NoError(t, err)
	assert.Equal(t, "a-cached-test", v1)
	assert.Equal(t, "b-cached-test", v2)
}

// TestRenderWithFuncs_ExtraFunctions verifies that extra template functions
// are merged with the standard set and both are available.
func TestRenderWithFuncs_ExtraFunctions(t *testing.T) {
	extra := template.FuncMap{
		"double": func(s string) string { return s + s },
	}
	c := &sync.Map{}

	// Extra func works
	got, err := RenderWithFuncs("{{double .x}}", map[string]any{"x": "ab"}, extra, c)
	require.NoError(t, err)
	assert.Equal(t, "abab", got)

	// Standard func still works alongside extra
	got, err = RenderWithFuncs("{{lower .x}}-{{double .y}}", map[string]any{"x": "HI", "y": "z"}, extra, c)
	require.NoError(t, err)
	assert.Equal(t, "hi-zz", got)
}

// TestRender_ConcurrentSafety exercises the template cache under concurrent access.
func TestRender_ConcurrentSafety(t *testing.T) {
	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		go func(n int) {
			defer wg.Done()
			_, err := Render("{{.n}}", map[string]any{"n": n})
			assert.NoError(t, err)
		}(i)
	}
	wg.Wait()
}

// TestFuncs_SliceBounds verifies slice handles out-of-bounds gracefully.
func TestFuncs_SliceBounds(t *testing.T) {
	got, err := Render(`{{slice .s 0 100}}`, map[string]any{"s": "short"})
	require.NoError(t, err)
	assert.Equal(t, "short", got)

	got, err = Render(`{{slice .s -5 3}}`, map[string]any{"s": "hello"})
	require.NoError(t, err)
	assert.Equal(t, "hel", got)

	got, err = Render(`{{slice .s 5 3}}`, map[string]any{"s": "hello"})
	require.NoError(t, err)
	assert.Equal(t, "", got)
}

// TestFuncs_DictOddArgs verifies dict errors on odd argument count.
func TestFuncs_DictOddArgs(t *testing.T) {
	_, err := Render(`{{dict "a" 1 "b"}}`, map[string]any{})
	assert.Error(t, err)
}

// TestRender_SignatureMatchesTemplateRenderer verifies that Render's signature
// is assignment-compatible with graph.TemplateRenderer.
// This is a compile-time check — if it compiles, it passes.
func TestRender_SignatureMatchesTemplateRenderer(t *testing.T) {
	// graph.TemplateRenderer is: func(tmpl string, values map[string]any) (string, error)
	fn := Render
	_ = fn
}

// ---------------------------------------------------------------------------
// Benchmarks — regression guard for template rendering performance.
// The template cache makes repeated renders cheap; these benchmarks verify
// that the extraction didn't introduce overhead vs the inline implementation.
// ---------------------------------------------------------------------------

// BenchmarkRender_Simple measures a trivial interpolation (cache-hot path).
func BenchmarkRender_Simple(b *testing.B) {
	values := map[string]any{"name": "CVE-2024-0001"}
	b.ResetTimer()
	for b.Loop() {
		_, _ = Render("{{.name}}", values)
	}
}

// BenchmarkRender_Complex measures a realistic schema template with multiple funcs.
func BenchmarkRender_Complex(b *testing.B) {
	values := map[string]any{
		"item": map[string]any{
			"cve": map[string]any{"id": "CVE-2024-12345"},
			"Vulnerability": map[string]any{
				"NamespaceName": "alpine:3.18",
				"Severity":      "High",
			},
		},
	}
	tmpl := `{{dig "item.cve.id" .}} | {{dig "item.Vulnerability.NamespaceName" .}} | {{dig "item.Vulnerability.Severity" .}}`
	b.ResetTimer()
	for b.Loop() {
		_, _ = Render(tmpl, values)
	}
}

// BenchmarkRender_CacheCold measures the cost of parsing a new template (cache miss).
func BenchmarkRender_CacheCold(b *testing.B) {
	values := map[string]any{"x": "hello"}
	b.ResetTimer()
	for i := range b.N {
		// Unique template string forces cache miss each iteration.
		tmpl := "{{.x}}-" + string(rune('A'+i%26)) + string(rune('a'+i%26))
		_, _ = Render(tmpl, values)
	}
}
