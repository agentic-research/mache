package writeback

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFormatGoBuffer_FormatsGo(t *testing.T) {
	// gofumpt enforces consistent formatting — use a full Go file
	// with inconsistent spacing that gofumpt will fix
	input := []byte("package main\n\nfunc A()  {\nreturn\n}\n")
	got := FormatGoBuffer(input, "main.go")
	expected := "package main\n\nfunc A() {\n\treturn\n}\n"
	assert.Equal(t, expected, string(got))
}

func TestFormatGoBuffer_NonGoPassthrough(t *testing.T) {
	input := []byte("def foo():\n  pass\n")
	got := FormatGoBuffer(input, "main.py")
	assert.Equal(t, input, got, "non-Go files should pass through unchanged")
}

func TestFormatGoBuffer_InvalidGoPassthrough(t *testing.T) {
	input := []byte("func broken {{{")
	got := FormatGoBuffer(input, "main.go")
	assert.Equal(t, input, got, "unparseable Go should return original buffer")
}
