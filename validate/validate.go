// Package validate exposes syntax validation for external consumers.
//
// This is the public API for mache's AST validation. Since mache-73b885 it is
// backed by the pinned leyline daemon's `validate` op (ley-line-open >=
// v0.8.0) instead of in-process tree-sitter. As of v0.8.0 the daemon
// validates EVERY mache registry language except CUE (no tree-sitter-0.26
// grammar exists); HCL/Terraform validates in-process via hclsyntax (a fast,
// offline path — the daemon covers it too). Only .cue and unrecognized
// extensions pass through with no error. A leyline daemon is acquired lazily
// on first use (see internal/leyline ValidateContent for the latency
// profile); daemon-acquisition failures surface as ordinary errors, never as
// false "valid" results.
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

// Content validates content for the language inferred from filePath's
// extension. Returns nil if the AST is clean or the extension is not
// validated (pass-through).
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

// SupportedExtension returns true if the file extension is validated by the
// leyline daemon's validate op.
func SupportedExtension(filePath string) bool {
	return writeback.SupportedPath(filePath)
}
