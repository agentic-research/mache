package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEscapeLikeMeta(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"MemoryStore", "MemoryStore"},
		{"normal_token", `normal\_token`},
		{"has%wildcard", `has\%wildcard`},
		{"has_single", `has\_single`},
		{`has\backslash`, `has\\backslash`},
		{"has'quote", "has'quote"},               // quotes NOT escaped
		{`'; DROP TABLE --`, `'; DROP TABLE --`}, // quotes NOT escaped
		{`%' OR '1'='1`, `\%' OR '1'='1`},        // only LIKE metas escaped
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.want, escapeLikeMeta(tt.input))
		})
	}
}

func TestEscapeLikePattern(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"MemoryStore", "MemoryStore"},
		{"normal_token", `normal\_token`},
		{"has%wildcard", `has\%wildcard`},
		{"has_single", `has\_single`},
		{`has\backslash`, `has\\backslash`},
		{"has'quote", "has''quote"},
		{`'; DROP TABLE --`, `''; DROP TABLE --`},
		{`%' OR '1'='1`, `\%'' OR ''1''=''1`},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.want, escapeLikePattern(tt.input))
		})
	}
}
