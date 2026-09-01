package smells

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFindSmellsCmd_ReturnsTheRegisteredCommand pins registration hook #1:
// cmd/register.go wires whatever this returns onto rootCmd, so it must be the
// live command carrying the flag set — not a copy that silently drops flags.
func TestFindSmellsCmd_ReturnsTheRegisteredCommand(t *testing.T) {
	c := FindSmellsCmd()
	require.NotNil(t, c)
	assert.Equal(t, "find-smells", c.Use)
	assert.Same(t, findSmellsCmd, c, "must be the live command, not a clone")
	assert.NotNil(t, c.Flags().Lookup("db"), "flag set must ride along")
}

// TestRunFindSmells_OptionsDriveTheCLIPath pins the options entry: same
// engine, same envelope as the flag-driven CLI (the parity gate in cmd
// depends on this equivalence), defaults applied for zero values, and the
// singleton flag state restored afterwards.
func TestRunFindSmells_OptionsDriveTheCLIPath(t *testing.T) {
	path, _ := fixturedb.New(t, fixturedb.Standalone).
		Construct("pkg/Dead", fixturedb.Where{Source: "dead.go"}).
		Def("Dead", "pkg/Dead", "").
		Build()

	before := findSmellsLimit
	var buf bytes.Buffer
	code, err := RunFindSmells(FindSmellsOptions{
		DB: path, Rule: "dead_code", Out: &buf, Err: &buf,
	})
	require.NoError(t, err)
	require.Zero(t, code, "dead_code ships at warn; default --fail-on=error must not gate: %s", buf.String())

	var resp struct {
		Findings []struct {
			SourceID string `json:"source_id"`
		} `json:"findings"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &resp), "zero-value Format must default to json")
	require.Len(t, resp.Findings, 1, "the unreferenced def must be found")
	assert.Equal(t, before, findSmellsLimit, "the CLI singleton state must be restored")
}
