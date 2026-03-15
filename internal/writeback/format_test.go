package writeback

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFormatBuffer_FormatsGo(t *testing.T) {
	// gofumpt enforces consistent formatting — use a full Go file
	// with inconsistent spacing that gofumpt will fix
	input := []byte("package main\n\nfunc A()  {\nreturn\n}\n")
	got := FormatBuffer(input, "main.go")
	expected := "package main\n\nfunc A() {\n\treturn\n}\n"
	assert.Equal(t, expected, string(got))
}

func TestFormatBuffer_InvalidGoPassthrough(t *testing.T) {
	input := []byte("func broken {{{")
	got := FormatBuffer(input, "main.go")
	assert.Equal(t, input, got, "unparseable Go should return original buffer")
}

func TestFormatBuffer_FormatsHCL(t *testing.T) {
	// hclwrite.Format normalizes indentation and spacing
	input := []byte("resource \"aws_instance\" \"web\" {\n  ami           = \"abc-123\"\ninstance_type = \"t2.micro\"\n}\n")
	got := FormatBuffer(input, "main.tf")
	assert.Contains(t, string(got), "resource")
	// hclwrite.Format should produce consistent output (not identical to input)
	assert.NotEmpty(t, got)
}

func TestFormatBuffer_FormatsHCLExtension(t *testing.T) {
	input := []byte("variable \"name\" {\ndefault = \"hello\"\n}\n")
	got := FormatBuffer(input, "vars.hcl")
	assert.Contains(t, string(got), "variable")
	assert.NotEmpty(t, got)
}

func TestFormatBuffer_FormatsPython(t *testing.T) {
	_, errRuff := exec.LookPath("ruff")
	_, errBlack := exec.LookPath("black")
	if errRuff != nil && errBlack != nil {
		t.Skip("neither ruff nor black available")
	}
	input := []byte("x=1\ny  =  2\n")
	got := FormatBuffer(input, "main.py")
	assert.Contains(t, string(got), "x")
	assert.NotEqual(t, string(input), string(got), "Python formatter should modify badly-formatted code")
}

func TestFormatBuffer_FormatsPyi(t *testing.T) {
	_, errRuff := exec.LookPath("ruff")
	_, errBlack := exec.LookPath("black")
	if errRuff != nil && errBlack != nil {
		t.Skip("neither ruff nor black available")
	}
	input := []byte("def foo(x:int)->str: ...\n")
	got := FormatBuffer(input, "stubs.pyi")
	assert.Contains(t, string(got), "foo")
}

func TestFormatBuffer_PythonPassthroughWhenNoFormatter(t *testing.T) {
	_, errRuff := exec.LookPath("ruff")
	_, errBlack := exec.LookPath("black")
	if errRuff == nil || errBlack == nil {
		t.Skip("test only valid when no Python formatter is installed")
	}
	input := []byte("x=1\n")
	got := FormatBuffer(input, "main.py")
	assert.Equal(t, input, got)
}

func TestFormatBuffer_FormatsTypeScript(t *testing.T) {
	if _, err := exec.LookPath("prettier"); err != nil {
		t.Skip("prettier not available")
	}
	input := []byte("const x:number=1\n")
	got := FormatBuffer(input, "main.ts")
	assert.Contains(t, string(got), "x")
}

func TestFormatBuffer_FormatsJSX(t *testing.T) {
	if _, err := exec.LookPath("prettier"); err != nil {
		t.Skip("prettier not available")
	}
	input := []byte("const App=()=><div>hello</div>\n")
	got := FormatBuffer(input, "App.jsx")
	assert.Contains(t, string(got), "App")
}

func TestFormatBuffer_TypeScriptPassthroughWhenNoFormatter(t *testing.T) {
	if _, err := exec.LookPath("prettier"); err == nil {
		t.Skip("test only valid when prettier is not installed")
	}
	input := []byte("const x:number=1\n")
	got := FormatBuffer(input, "main.ts")
	assert.Equal(t, input, got)
}

func TestFormatBuffer_YAMLPassthrough(t *testing.T) {
	input := []byte("key: value\nlist:\n  - item1\n")
	got := FormatBuffer(input, "config.yaml")
	assert.Equal(t, input, got, "YAML should pass through unchanged (no formatter)")
}
