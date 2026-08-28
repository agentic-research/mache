package cmd

import (
	"github.com/agentic-research/mache/internal/smells"
)

// Extracted-package command registration (mache-96c378). Each extracted
// package defines its own cobra command and exports a constructor; THIS file
// is the single place that decides what exists on the mache CLI. The
// packages never import cmd — the arrow points only downward.
func init() {
	rootCmd.AddCommand(smells.FindSmellsCmd())
}
