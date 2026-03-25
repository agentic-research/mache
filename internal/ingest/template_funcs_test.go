package ingest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// replace — strings.ReplaceAll(s, old, new)
// ---------------------------------------------------------------------------

func TestTemplateFuncs_Replace(t *testing.T) {
	out, err := RenderTemplate(`{{replace .s ":" " "}}`, map[string]any{"s": "alpine:3.18"})
	require.NoError(t, err)
	assert.Equal(t, "alpine 3.18", out)
}

func TestTemplateFuncs_ReplaceMultiple(t *testing.T) {
	out, err := RenderTemplate(`{{replace .s "a" "o"}}`, map[string]any{"s": "banana"})
	require.NoError(t, err)
	assert.Equal(t, "bonono", out)
}

func TestTemplateFuncs_ReplaceNoMatch(t *testing.T) {
	out, err := RenderTemplate(`{{replace .s "x" "y"}}`, map[string]any{"s": "hello"})
	require.NoError(t, err)
	assert.Equal(t, "hello", out)
}

// ---------------------------------------------------------------------------
// lower — strings.ToLower(s)
// ---------------------------------------------------------------------------

func TestTemplateFuncs_Lower(t *testing.T) {
	out, err := RenderTemplate(`{{lower .s}}`, map[string]any{"s": "RHEL"})
	require.NoError(t, err)
	assert.Equal(t, "rhel", out)
}

func TestTemplateFuncs_LowerAlreadyLower(t *testing.T) {
	out, err := RenderTemplate(`{{lower .s}}`, map[string]any{"s": "debian"})
	require.NoError(t, err)
	assert.Equal(t, "debian", out)
}

// ---------------------------------------------------------------------------
// upper — strings.ToUpper(s)
// ---------------------------------------------------------------------------

func TestTemplateFuncs_Upper(t *testing.T) {
	out, err := RenderTemplate(`{{upper .s}}`, map[string]any{"s": "debian"})
	require.NoError(t, err)
	assert.Equal(t, "DEBIAN", out)
}

// ---------------------------------------------------------------------------
// title — first letter of each word uppercase
// ---------------------------------------------------------------------------

func TestTemplateFuncs_Title(t *testing.T) {
	out, err := RenderTemplate(`{{title .s}}`, map[string]any{"s": "amazon linux"})
	require.NoError(t, err)
	assert.Equal(t, "Amazon Linux", out)
}

func TestTemplateFuncs_TitleSingleWord(t *testing.T) {
	out, err := RenderTemplate(`{{title .s}}`, map[string]any{"s": "debian"})
	require.NoError(t, err)
	assert.Equal(t, "Debian", out)
}

// ---------------------------------------------------------------------------
// split — strings.Split(s, sep)
// ---------------------------------------------------------------------------

func TestTemplateFuncs_Split(t *testing.T) {
	// split returns a slice; use index to extract elements
	out, err := RenderTemplate(`{{index (split .s ":") 0}}`, map[string]any{"s": "alpine:3.18"})
	require.NoError(t, err)
	assert.Equal(t, "alpine", out)
}

func TestTemplateFuncs_SplitSecondElement(t *testing.T) {
	out, err := RenderTemplate(`{{index (split .s ":") 1}}`, map[string]any{"s": "alpine:3.18"})
	require.NoError(t, err)
	assert.Equal(t, "3.18", out)
}

// ---------------------------------------------------------------------------
// join — strings.Join(elems, sep)
// ---------------------------------------------------------------------------

func TestTemplateFuncs_Join(t *testing.T) {
	// join signature: join(sep, parts) — sep first so piped slices land as last arg
	out, err := RenderTemplate(`{{join ", " .parts}}`, map[string]any{
		"parts": []string{"a", "b", "c"},
	})
	require.NoError(t, err)
	assert.Equal(t, "a, b, c", out)
}

func TestTemplateFuncs_JoinAnySlice(t *testing.T) {
	// JSON-parsed slices are []any, not []string — join should handle both
	out, err := RenderTemplate(`{{join "-" .parts}}`, map[string]any{
		"parts": []any{"x", "y"},
	})
	require.NoError(t, err)
	assert.Equal(t, "x-y", out)
}

// ---------------------------------------------------------------------------
// Composition — chaining template funcs in a pipeline
// ---------------------------------------------------------------------------

func TestTemplateFuncs_ReplaceAndLower(t *testing.T) {
	// alpine:3.18 → replace ":" with " " → lower (already lower, but tests pipeline)
	out, err := RenderTemplate(`{{replace .s ":" " " | lower}}`, map[string]any{"s": "Alpine:3.18"})
	require.NoError(t, err)
	assert.Equal(t, "alpine 3.18", out)
}

func TestTemplateFuncs_SplitJoin(t *testing.T) {
	// "a:b:c" → split on ":" → join with ", "
	out, err := RenderTemplate(`{{split .s ":" | join ", "}}`, map[string]any{"s": "a:b:c"})
	require.NoError(t, err)
	assert.Equal(t, "a, b, c", out)
}

// ---------------------------------------------------------------------------
// hasPrefix / hasSuffix — strings.HasPrefix / HasSuffix
// ---------------------------------------------------------------------------

func TestTemplateFuncs_HasPrefix(t *testing.T) {
	out, err := RenderTemplate(`{{if hasPrefix .s "CVE"}}yes{{else}}no{{end}}`, map[string]any{"s": "CVE-2024-0001"})
	require.NoError(t, err)
	assert.Equal(t, "yes", out)
}

func TestTemplateFuncs_HasPrefixFalse(t *testing.T) {
	out, err := RenderTemplate(`{{if hasPrefix .s "CVE"}}yes{{else}}no{{end}}`, map[string]any{"s": "GHSA-1234"})
	require.NoError(t, err)
	assert.Equal(t, "no", out)
}

func TestTemplateFuncs_HasSuffix(t *testing.T) {
	out, err := RenderTemplate(`{{if hasSuffix .s ".go"}}go{{else}}other{{end}}`, map[string]any{"s": "main.go"})
	require.NoError(t, err)
	assert.Equal(t, "go", out)
}

// ---------------------------------------------------------------------------
// trimPrefix / trimSuffix — strings.TrimPrefix / TrimSuffix
// ---------------------------------------------------------------------------

func TestTemplateFuncs_TrimPrefix(t *testing.T) {
	out, err := RenderTemplate(`{{trimPrefix .s "pkg/"}}`, map[string]any{"s": "pkg/auth/login.go"})
	require.NoError(t, err)
	assert.Equal(t, "auth/login.go", out)
}

func TestTemplateFuncs_TrimSuffix(t *testing.T) {
	out, err := RenderTemplate(`{{trimSuffix .s ".go"}}`, map[string]any{"s": "main.go"})
	require.NoError(t, err)
	assert.Equal(t, "main", out)
}

// ---------------------------------------------------------------------------
// dict — construct a map inline
// ---------------------------------------------------------------------------

func TestTemplateFuncs_Dict(t *testing.T) {
	out, err := RenderTemplate(`{{dict "name" "curl" "version" "7.88" | json}}`, map[string]any{})
	require.NoError(t, err)
	assert.JSONEq(t, `{"name":"curl","version":"7.88"}`, out)
}

func TestTemplateFuncs_DictWithTemplateValues(t *testing.T) {
	out, err := RenderTemplate(`{{dict "PkgName" .name "FixedVersion" .ver | json}}`, map[string]any{
		"name": "openssl",
		"ver":  "3.0.1",
	})
	require.NoError(t, err)
	assert.JSONEq(t, `{"PkgName":"openssl","FixedVersion":"3.0.1"}`, out)
}

func TestTemplateFuncs_DictEmpty(t *testing.T) {
	out, err := RenderTemplate(`{{dict | json}}`, map[string]any{})
	require.NoError(t, err)
	assert.Equal(t, "{}", out)
}

func TestTemplateFuncs_DictWithIntValue(t *testing.T) {
	out, err := RenderTemplate(`{{dict "Severity" 4 "Status" 3 | json}}`, map[string]any{})
	require.NoError(t, err)
	assert.JSONEq(t, `{"Severity":4,"Status":3}`, out)
}

// ---------------------------------------------------------------------------
// lookup — enum/key-value mapping
// ---------------------------------------------------------------------------

func TestTemplateFuncs_Lookup(t *testing.T) {
	out, err := RenderTemplate(`{{lookup .sev "Critical" "4" "High" "3" "Medium" "2" "Low" "1"}}`, map[string]any{"sev": "High"})
	require.NoError(t, err)
	assert.Equal(t, "3", out)
}

func TestTemplateFuncs_LookupDefault(t *testing.T) {
	// Odd number of args after value → last arg is default
	out, err := RenderTemplate(`{{lookup .sev "Critical" "4" "High" "3" "0"}}`, map[string]any{"sev": "Unknown"})
	require.NoError(t, err)
	assert.Equal(t, "0", out)
}

func TestTemplateFuncs_LookupNoMatch(t *testing.T) {
	// Even args, no match → empty string
	out, err := RenderTemplate(`{{lookup .sev "Critical" "4" "High" "3"}}`, map[string]any{"sev": "Low"})
	require.NoError(t, err)
	assert.Equal(t, "", out)
}

func TestTemplateFuncs_LookupIntValues(t *testing.T) {
	out, err := RenderTemplate(`{{lookup .sev "Critical" 4 "High" 3 "Medium" 2 "Low" 1 0}}`, map[string]any{"sev": "Critical"})
	require.NoError(t, err)
	assert.Equal(t, "4", out)
}

// ---------------------------------------------------------------------------
// default — fallback for empty/nil values
// ---------------------------------------------------------------------------

func TestTemplateFuncs_Default(t *testing.T) {
	out, err := RenderTemplate(`{{default .name "unknown"}}`, map[string]any{"name": "curl"})
	require.NoError(t, err)
	assert.Equal(t, "curl", out)
}

func TestTemplateFuncs_DefaultEmpty(t *testing.T) {
	out, err := RenderTemplate(`{{default .name "unknown"}}`, map[string]any{"name": ""})
	require.NoError(t, err)
	assert.Equal(t, "unknown", out)
}

func TestTemplateFuncs_DefaultNil(t *testing.T) {
	out, err := RenderTemplate(`{{default .name "unknown"}}`, map[string]any{})
	require.NoError(t, err)
	assert.Equal(t, "unknown", out)
}

// ---------------------------------------------------------------------------
// Composition — dict + lookup + json pipeline for trivy Advisory shape
// ---------------------------------------------------------------------------

func TestTemplateFuncs_TrivyAdvisoryShape(t *testing.T) {
	// Simulates the trivy Advisory JSON construction from venturi GitHub Advisory fields
	tmpl := `{{dict "FixedVersion" .ver "Severity" (lookup .sev "Critical" 4 "High" 3 "Medium" 2 "Low" 1 0) | json}}`
	out, err := RenderTemplate(tmpl, map[string]any{
		"ver": "3.0.1",
		"sev": "High",
	})
	require.NoError(t, err)
	assert.JSONEq(t, `{"FixedVersion":"3.0.1","Severity":3}`, out)
}
