package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize a project for mache MCP serving",
	Long: `Creates .mache.json and .claude/mcp.json in the current directory.
This lets 'mache serve' (zero args) know what data to project, and
registers mache as an MCP server for Claude Code.`,
	Args: cobra.NoArgs,
	RunE: runInit,
}

// initOpts holds all init configuration, avoiding package-level flag state.
type initOpts struct {
	Force  bool
	Global bool
	Schema string
	Source string
}

var initFlags initOpts

func init() {
	initCmd.Flags().BoolVar(&initFlags.Force, "force", false, "Overwrite existing .mache.json")
	initCmd.Flags().BoolVar(&initFlags.Global, "global", false, "Install mache MCP server globally in ~/.claude/")
	initCmd.Flags().StringVar(&initFlags.Schema, "schema", "", "Schema preset or path (auto-detected if omitted)")
	initCmd.Flags().StringVar(&initFlags.Source, "source", ".", "Data source path")
	rootCmd.AddCommand(initCmd)
}

func runInit(cmd *cobra.Command, _ []string) error {
	// Resolve mache binary path (needed for both modes)
	macheCmd, err := os.Executable()
	if err != nil {
		macheCmd = "mache"
	}

	w := cmd.OutOrStdout()
	return execInit(w, macheCmd, initFlags)
}

func execInit(w io.Writer, macheCmd string, opts initOpts) error {
	if opts.Global {
		return execInitGlobal(w, macheCmd)
	}
	return execInitProject(w, macheCmd, opts)
}

func execInitGlobal(w io.Writer, macheCmd string) error {
	_, _ = fmt.Fprintln(w, "Registering mache MCP server with detected editors...")
	_, _ = fmt.Fprintln(w)

	registerAllEditors(w, macheCmd)

	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "mache is now available as an MCP server. Restart your editor to activate.")
	_, _ = fmt.Fprintln(w, "Run 'mache init' (without --global) in a project to configure what it serves.")
	return nil
}

func execInitProject(w io.Writer, macheCmd string, opts initOpts) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}

	// Check for existing config
	configPath := ConfigFileName
	if _, err := os.Stat(configPath); err == nil && !opts.Force {
		return fmt.Errorf("%s already exists (use --force to overwrite)", configPath)
	}

	// Auto-detect schema if not provided
	schema := opts.Schema
	if schema == "" {
		schema = detectProjectType(cwd)
	}

	// Build config
	src := SourceConfig{Path: opts.Source}
	if schema != "" {
		src.Schema = schema
	}
	cfg := ProjectConfig{Sources: []SourceConfig{src}}

	// Write .mache.json
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(configPath, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", configPath, err)
	}
	_, _ = fmt.Fprintf(w, "Created %s\n", configPath)

	// Write/merge .claude/mcp.json
	if err := writeClaudeMCPConfig(cwd, macheCmd); err != nil {
		return fmt.Errorf("write claude mcp config: %w", err)
	}
	_, _ = fmt.Fprintf(w, "Updated .claude/mcp.json\n")

	// Write/merge .claude/CLAUDE.md
	if err := writeClaudeMD(cwd, schema); err != nil {
		return fmt.Errorf("write CLAUDE.md: %w", err)
	}
	_, _ = fmt.Fprintf(w, "Updated .claude/CLAUDE.md\n")

	// Summary
	_, _ = fmt.Fprintln(w)
	if schema != "" {
		_, _ = fmt.Fprintf(w, "  Schema: %s (preset)\n", schema)
	} else {
		_, _ = fmt.Fprintf(w, "  Schema: (none — will use default)\n")
	}
	_, _ = fmt.Fprintf(w, "  Source: %s\n", opts.Source)
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Run 'mache serve' to start the MCP server.")

	return nil
}
