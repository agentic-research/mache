package writeback

import (
	"strings"

	"mvdan.cc/gofumpt/format"
)

// FormatGoBuffer formats Go source code in-memory using gofumpt.
// Returns the formatted buffer, or the original buffer unchanged if
// the file is not a Go file or formatting fails.
func FormatGoBuffer(content []byte, filePath string) []byte {
	if !strings.HasSuffix(filePath, ".go") {
		return content
	}
	formatted, err := format.Source(content, format.Options{})
	if err != nil {
		return content // formatting failed — return original
	}
	return formatted
}
