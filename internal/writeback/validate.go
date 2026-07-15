package writeback

// Syntax validation for the write-back pipeline.
//
// Since mache-73b885 this package holds NO in-process tree-sitter: validation
// is delegated to the pinned leyline daemon's `validate` op (ley-line-open >=
// v0.7.8), which runs the same grammars the `_ast` producer uses. The daemon
// is acquired lazily per call via leyline.ValidateContent (DiscoverOrStart);
// see that function's doc for the latency profile (sub-ms with a live daemon,
// a one-off spawn cost on the first write otherwise).
//
// Language coverage CHANGED with the migration: the daemon validates the
// extension keys in leylineValidateLangs below. Extensions that mache's old
// in-process grammar set covered but the daemon does not (e.g. .tf/.hcl,
// .yaml, .sql, .md, .toml) now PASS THROUGH unvalidated — same contract as an
// unknown extension. For HCL/Terraform, FormatBuffer's hclwrite step remains
// the only structural check on the write path.

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/agentic-research/mache/internal/leyline"
)

// ValidationError contains structured information about a syntax error.
type ValidationError struct {
	FilePath string
	Line     uint32 // 0-indexed
	Column   uint32 // 0-indexed (byte offset within the line)
	Message  string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s:%d:%d: %s", e.FilePath, e.Line+1, e.Column+1, e.Message)
}

// leylineValidateLangs is the set of extension keys the pinned leyline
// daemon's validate op accepts (ley-line-open rs/ll-open/fs/src/validate.rs
// language_for_extension). The key doubles as the wire `language` value.
var leylineValidateLangs = map[string]bool{
	"go":  true,
	"py":  true,
	"js":  true,
	"ts":  true,
	"tsx": true,
	"rs":  true,
	"ex":  true,
	"exs": true,
}

// langKeyForPath maps a file path to the leyline validate language key, or ""
// when the extension is not validated (pass-through).
func langKeyForPath(filePath string) string {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(filePath)), ".")
	if leylineValidateLangs[ext] {
		return ext
	}
	return ""
}

// SupportedPath reports whether filePath's extension is syntax-validated on
// the write path (i.e. recognized by the leyline daemon's validate op).
func SupportedPath(filePath string) bool {
	return langKeyForPath(filePath) != ""
}

// Validate checks content for syntax errors via the leyline daemon and
// returns a *ValidationError for the first ERROR/MISSING node. Files whose
// extension the daemon does not validate pass through (returns nil).
// Daemon-acquisition failures and daemon-too-old responses are returned as
// ordinary (non-ValidationError) errors so callers can distinguish "your code
// is broken" from "the validator is unavailable".
func Validate(content []byte, filePath string) error {
	_, err := validateRemote(content, filePath, false)
	return err
}

// ValidateWithAST is Validate plus AST rows from the SAME parse: for
// languages with an emit_ast-capable extractor that mache lints (Go today),
// a clean validation also returns the daemon's SQL-shaped AST payload so the
// linter can run without a second parse. For every other validated language
// it behaves exactly like Validate and returns (nil, nil) on success.
// Pass-through extensions return (nil, nil).
func ValidateWithAST(content []byte, filePath string) (*leyline.ASTPayload, error) {
	return validateRemote(content, filePath, true)
}

// validateRemote runs one daemon validate round trip. wantAST requests
// emit_ast, which is only sent for Go — the only language mache has AST lint
// rules for, and a member of the daemon extractor's supported subset (the
// emit_ast pipeline covers fewer languages than the validator; requesting it
// for an uncovered language is a daemon-side hard error).
func validateRemote(content []byte, filePath string, wantAST bool) (*leyline.ASTPayload, error) {
	key := langKeyForPath(filePath)
	if key == "" {
		return nil, nil // not validated — pass through
	}
	emit := wantAST && key == "go"

	res, err := leyline.ValidateContent(content, key, filePath, emit)
	if err != nil {
		return nil, fmt.Errorf("validate %s: %w", filePath, err)
	}
	if !res.OK {
		if len(res.Errors) > 0 {
			first := res.Errors[0]
			return nil, &ValidationError{
				FilePath: filePath,
				Line:     first.Row,
				Column:   first.Col,
				Message:  first.Message,
			}
		}
		// Defensive: the daemon reports ok=false with a populated errors
		// array; an empty one still must not validate the write.
		return nil, &ValidationError{FilePath: filePath, Message: "AST contains errors"}
	}
	return res.AST, nil
}

// ASTErrors returns all ERROR/MISSING node locations in the content for
// diagnostic reporting (0-based positions, straight off the daemon wire).
// Returns nil if the content is clean, the extension is not validated, or the
// daemon is unavailable — this is the diagnostic-rendering flavor and has no
// error channel, matching the historical contract.
func ASTErrors(content []byte, filePath string) []ValidationError {
	key := langKeyForPath(filePath)
	if key == "" {
		return nil
	}
	res, err := leyline.ValidateContent(content, key, filePath, false)
	if err != nil || res.OK {
		return nil
	}
	errs := make([]ValidationError, 0, len(res.Errors))
	for _, e := range res.Errors {
		errs = append(errs, ValidationError{
			FilePath: filePath,
			Line:     e.Row,
			Column:   e.Col,
			Message:  e.Message,
		})
	}
	return errs
}
