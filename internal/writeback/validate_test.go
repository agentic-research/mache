package writeback

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidate_ValidGo(t *testing.T) {
	src := []byte(`package main

func hello() string {
	return "world"
}
`)
	err := Validate(src, "test.go")
	assert.NoError(t, err)
}

func TestValidate_BrokenGo(t *testing.T) {
	src := []byte(`package main

func hello() string {
	return "world"
// missing closing brace
`)
	err := Validate(src, "test.go")
	require.Error(t, err)

	var ve *ValidationError
	require.ErrorAs(t, err, &ve)
	assert.Equal(t, "test.go", ve.FilePath)
	assert.Contains(t, ve.Message, "syntax error")
}

func TestValidate_ValidPython(t *testing.T) {
	src := []byte(`def hello():
    return "world"
`)
	err := Validate(src, "test.py")
	assert.NoError(t, err)
}

func TestValidate_BrokenPython(t *testing.T) {
	src := []byte(`def hello(
    return "world"
`)
	err := Validate(src, "test.py")
	require.Error(t, err)
}

func TestValidate_ValidJS(t *testing.T) {
	src := []byte(`function hello() { return "world"; }`)
	err := Validate(src, "test.js")
	assert.NoError(t, err)
}

func TestValidate_UnknownExtension_PassThrough(t *testing.T) {
	// Unknown extensions should pass through without error
	src := []byte(`this is not valid code in any language {{{`)
	err := Validate(src, "test.txt")
	assert.NoError(t, err)
}

func TestValidate_EmptyContent(t *testing.T) {
	err := Validate([]byte{}, "test.go")
	assert.NoError(t, err)
}

func TestASTErrors_BrokenGo(t *testing.T) {
	src := []byte(`package main

func hello() {
	x :=
}
`)
	errs := ASTErrors(src, "test.go")
	require.NotEmpty(t, errs)
	assert.Equal(t, "test.go", errs[0].FilePath)
}

func TestASTErrors_ValidGo_ReturnsNil(t *testing.T) {
	src := []byte(`package main

func hello() {}
`)
	errs := ASTErrors(src, "test.go")
	assert.Nil(t, errs)
}

func TestASTErrors_UnknownExtension_ReturnsNil(t *testing.T) {
	errs := ASTErrors([]byte(`broken {{{`), "test.txt")
	assert.Nil(t, errs)
}
