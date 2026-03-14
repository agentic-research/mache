package writeback

import (
	"bytes"
	"os/exec"
	"strings"

	"github.com/hashicorp/hcl/v2/hclwrite"
	"mvdan.cc/gofumpt/format"
)

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
	out, err := runFormatter(path, []string{"--stdin-filepath", filePath}, content)
	if err != nil {
		return content
	}
	return out
}

// runFormatter executes an external formatter, piping content via stdin and
// returning stdout. Returns an error if the command fails.
func runFormatter(path string, args []string, content []byte) ([]byte, error) {
	cmd := exec.Command(path, args...)
	cmd.Stdin = bytes.NewReader(content)
	return cmd.Output()
}
