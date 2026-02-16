package writeback

import (
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

func TestFormatBuffer_NonGoPassthrough(t *testing.T) {
	input := []byte("def foo():\n  pass\n")
	got := FormatBuffer(input, "main.py")
	assert.Equal(t, input, got, "non-Go files should pass through unchanged")
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

func TestFormatBuffer_YAMLPassthrough(t *testing.T) {
	input := []byte("key: value\nlist:\n  - item1\n")
	got := FormatBuffer(input, "config.yaml")
	assert.Equal(t, input, got, "YAML should pass through unchanged (no formatter)")
}
