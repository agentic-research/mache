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
// Language coverage: since ley-line-open v0.8.0 the daemon validates EVERY
// mache registry language except cue (no tree-sitter-0.26 cue grammar), so
// daemonValidates gates on "recognized source extension, not cue, not HCL".
// HCL/Terraform validates IN-PROCESS via hclsyntax (hclwrite.Format is a
// token formatter, NOT a validator — it mangles broken input; the daemon
// also validates HCL now, but the in-process path is faster and needs no
// daemon). Only cue and unrecognized extensions pass through unvalidated.
// The daemon identifies the language from the path, so mache keeps no
// per-extension language-id map (the old one missed .cc/.cxx/.mjs aliases).

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"

	"github.com/agentic-research/mache/internal/lang"
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

// daemonValidates reports whether filePath is syntax-validated by the leyline
// daemon (as opposed to in-process HCL, or pass-through). Since ley-line-open
// v0.8.0 the daemon validates every registry language EXCEPT cue (no
// tree-sitter-0.26 cue grammar exists), so the gate is "recognized source
// extension, not cue, not HCL" — the language itself is identified by the
// daemon from the path, so mache never maintains a per-extension language-id
// map (the old map missed C++'s .cc/.cxx/.hpp aliases, JS's .mjs, etc.).
func daemonValidates(filePath string) bool {
	l := lang.ForPath(filePath)
	if l == nil {
		return false // unrecognized extension → pass through
	}
	if l.Name == "cue" {
		return false // no tree-sitter-0.26 cue grammar; daemon rejects it
	}
	return !isHCLPath(filePath) // HCL validates in-process (below)
}

// SupportedPath reports whether filePath's extension is syntax-validated on
// write-back — by the leyline daemon (every registry language except cue,
// since ley-line-open v0.8.0) or in-process (HCL/Terraform via hclsyntax).
// Only cue and unrecognized extensions pass through unvalidated.
func SupportedPath(filePath string) bool {
	return isHCLPath(filePath) || daemonValidates(filePath)
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
	// HCL/Terraform validates IN-PROCESS via hclsyntax (pure Go, same
	// hashicorp/hcl module hclwrite comes from) — restoring the pre-73b885
	// draft behavior. Without this, broken HCL passed through unvalidated
	// AND FormatBuffer's hclwrite.Format (a token formatter, NOT a
	// validator) MANGLED it before splicing to disk (#527 review).
	if isHCLPath(filePath) {
		return nil, validateHCL(content, filePath)
	}
	if !daemonValidates(filePath) {
		return nil, nil // cue / unrecognized extension — pass through
	}
	// emit_ast is Go-only (the sole language mache has AST lint rules for).
	// language="" → the daemon infers it from the path, which handles every
	// extension alias (.cc/.cxx/.mjs/...) without a per-extension map.
	emit := wantAST && lang.ForPath(filePath).Name == "go"

	res, err := leyline.ValidateContent(content, "", filePath, emit)
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

// isHCLPath reports whether filePath is HCL/Terraform — validated
// in-process (see validateRemote) rather than via the leyline daemon.
func isHCLPath(filePath string) bool {
	switch strings.ToLower(filepath.Ext(filePath)) {
	case ".tf", ".hcl":
		return true
	}
	return false
}

// validateHCL syntax-checks HCL/Terraform content with hclsyntax.ParseConfig.
// hcl positions are 1-based; ValidationError is 0-based (Error() re-renders
// 1-based), so subtract one. Returns the first error diagnostic as a
// *ValidationError, matching the Go path's first-error contract.
func validateHCL(content []byte, filePath string) error {
	errs := hclErrors(content, filePath)
	if len(errs) == 0 {
		return nil
	}
	first := errs[0]
	return &first
}

// hclErrors returns every hclsyntax error diagnostic as a ValidationError
// (0-based positions; hcl's are 1-based). Shared by validateHCL (first-error
// contract) and ASTErrors (all-errors diagnostics flavor).
func hclErrors(content []byte, filePath string) []ValidationError {
	_, diags := hclsyntax.ParseConfig(content, filePath, hcl.InitialPos)
	if !diags.HasErrors() {
		return nil
	}
	var errs []ValidationError
	for _, d := range diags {
		if d.Severity != hcl.DiagError {
			continue
		}
		ve := ValidationError{FilePath: filePath, Message: d.Summary}
		if d.Subject != nil {
			if d.Subject.Start.Line > 0 {
				ve.Line = uint32(d.Subject.Start.Line - 1) // #nosec G115 -- hcl lines are small positive ints
			}
			if d.Subject.Start.Column > 0 {
				ve.Column = uint32(d.Subject.Start.Column - 1) // #nosec G115
			}
		}
		errs = append(errs, ve)
	}
	if len(errs) == 0 {
		errs = append(errs, ValidationError{FilePath: filePath, Message: "HCL contains errors"})
	}
	return errs
}

// ASTErrors returns all ERROR/MISSING node locations in the content for
// diagnostic reporting (0-based positions, straight off the daemon wire).
// Returns nil if the content is clean, the extension is not validated, or the
// daemon is unavailable — this is the diagnostic-rendering flavor and has no
// error channel, matching the historical contract.
func ASTErrors(content []byte, filePath string) []ValidationError {
	// HCL mirrors validateRemote's in-process branch — ALL error
	// diagnostics, not just the first (this is the diagnostics flavor).
	if isHCLPath(filePath) {
		return hclErrors(content, filePath)
	}
	if !daemonValidates(filePath) {
		return nil
	}
	res, err := leyline.ValidateContent(content, "", filePath, false)
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
