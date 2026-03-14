package writeback

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2/hclwrite"
	"mvdan.cc/gofumpt/format"
)

// formatterTimeout is the maximum time an external formatter process may run
// before being killed. Prevents hung prettier/ruff/black from blocking the
// write-back pipeline indefinitely.
const formatterTimeout = 10 * time.Second

// FormatBuffer formats source code in-memory based on file extension.
// Go files use gofumpt; HCL/Terraform files use hclwrite.Format.
// Python files use ruff (preferred) or black.
// TypeScript/JavaScript files use prettier.
// Returns the formatted buffer, or the original buffer unchanged if
// the file type has no formatter or formatting fails.
func FormatBuffer(content []byte, filePath string) []byte {
	lower := strings.ToLower(filePath)
	switch {
	case strings.HasSuffix(lower, ".go"):
		formatted, err := format.Source(content, format.Options{})
		if err != nil {
			return content
		}
		return formatted
	case strings.HasSuffix(lower, ".tf"), strings.HasSuffix(lower, ".hcl"):
		return hclwrite.Format(content)
	case strings.HasSuffix(lower, ".py"), strings.HasSuffix(lower, ".pyi"):
		return formatPython(content)
	case strings.HasSuffix(lower, ".ts"), strings.HasSuffix(lower, ".tsx"),
		strings.HasSuffix(lower, ".js"), strings.HasSuffix(lower, ".jsx"):
		return formatPrettier(content, filePath)
	default:
		return content
	}
}

// formatPython formats Python source via ruff (preferred) or black.
// Returns the original content if neither tool is available or formatting fails.
func formatPython(content []byte) []byte {
	if path, err := exec.LookPath("ruff"); err == nil {
		if out, err := runFormatter(path, []string{"format", "--stdin-filename", "f.py", "-"}, content); err == nil {
			return out
		}
	}
	if path, err := exec.LookPath("black"); err == nil {
		if out, err := runFormatter(path, []string{"-", "--quiet"}, content); err == nil {
			return out
		}
	}
	return content
}

// formatPrettier formats TypeScript/JavaScript source via prettier.
// Returns the original content if prettier is not available or formatting fails.
func formatPrettier(content []byte, filePath string) []byte {
	path, err := exec.LookPath("prettier")
	if err != nil {
		return content
	}
	// Prefix relative paths with "./" to prevent filenames starting with
	// "--" from being interpreted as flags by prettier.
	safePath := filePath
	if !strings.HasPrefix(safePath, "/") && !strings.HasPrefix(safePath, "./") {
		safePath = "./" + safePath
	}
	out, err := runFormatter(path, []string{"--stdin-filepath", safePath}, content)
	if err != nil {
		return content
	}
	return out
}

// runFormatter executes an external formatter, piping content via stdin and
// returning stdout. The process is killed after formatterTimeout to prevent
// hung formatters from blocking the write-back pipeline.
func runFormatter(path string, args []string, content []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), formatterTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdin = bytes.NewReader(content)
	return cmd.Output()
}
