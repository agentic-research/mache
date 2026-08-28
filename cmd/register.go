package cmd

import (
	"github.com/agentic-research/mache/internal/buildcache"
	"github.com/agentic-research/mache/internal/mcpserve"
	"github.com/agentic-research/mache/internal/smells"
)

// Extracted-package command registration (mache-96c378). Each extracted
// package defines its own cobra command and exports a constructor; THIS file
// is the single place that decides what exists on the mache CLI. The
// packages never import cmd — the arrow points only downward.
func init() {
	rootCmd.AddCommand(smells.FindSmellsCmd())
	rootCmd.AddCommand(buildcache.CacheCmd())
	rootCmd.AddCommand(mcpserve.ServeCmd())
	// The ldflags-stamped build version becomes the producer identity in
	// lockfiles/cache metadata. Pull-based on the buildcache side, so init
	// order is irrelevant (B24).
	buildcache.SetProducerVersion(Version)
	mcpserve.SetVersion(Version)
}
