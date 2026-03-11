// Package validate exposes tree-sitter syntax validation for external consumers.
//
// This is the public API for mache's AST validation. It supports Go, Python,
// JavaScript, TypeScript, SQL, HCL, YAML, and Rust via tree-sitter grammars.
//
// Usage:
//
//	err := validate.File("path/to/file.go")
//	errors := validate.FileErrors("path/to/file.go")
//	err := validate.Content([]byte("package main"), "main.go")
package validate

import (
	"github.com/agentic-research/mache/internal/writeback"
)

// ValidationError contains structured information about a syntax error.
type ValidationError = writeback.ValidationError

// Content parses content with tree-sitter for the language inferred from
// filePath's extension. Returns nil if the AST is clean or the language
// is unknown (pass-through).
func Content(content []byte, filePath string) error {
	return writeback.Validate(content, filePath)
}

// ContentErrors returns all AST error locations for diagnostic reporting.
// Returns nil if no errors or unknown language.
func ContentErrors(content []byte, filePath string) []ValidationError {
	return writeback.ASTErrors(content, filePath)
}

// File reads a file from disk and validates its AST.
func File(filePath string) error {
	content, err := readFile(filePath)
	if err != nil {
		return err
	}
	return Content(content, filePath)
}

// FileErrors reads a file from disk and returns all AST error locations.
func FileErrors(filePath string) []ValidationError {
	content, err := readFile(filePath)
	if err != nil {
		return nil
	}
	return ContentErrors(content, filePath)
}

// SupportedExtension returns true if the file extension is recognized
// by the tree-sitter grammar set.
func SupportedExtension(filePath string) bool {
	return writeback.LanguageForPath(filePath) != nil
}
