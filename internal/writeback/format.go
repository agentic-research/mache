package writeback

import (
	"strings"

	"github.com/hashicorp/hcl/v2/hclwrite"
	"mvdan.cc/gofumpt/format"
)

// FormatBuffer formats source code in-memory based on file extension.
// Go files use gofumpt; HCL/Terraform files use hclwrite.Format.
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
	default:
		return content
	}
}
