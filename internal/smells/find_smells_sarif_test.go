package smells

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFindSmellsCLI_SARIFRender asserts --format=sarif emits a valid
// SARIF 2.1.0 document: a single run, the mache driver, the rule in
// driver.rules[], and one result per finding with a 1-based region.
func TestFindSmellsCLI_SARIFRender(t *testing.T) {
	dbPath := writeSmellCLIFixture(t)

	saved := saveCLIFlags()
	defer saved.restore()
	findSmellsRule = "dead_code"
	findSmellsDBPath = dbPath
	findSmellsFormat = "sarif"

	var buf bytes.Buffer
	findSmellsCmd.SetOut(&buf)
	findSmellsCmd.SetErr(&buf)
	require.NoError(t, findSmellsCmd.RunE(findSmellsCmd, nil))

	var doc struct {
		Version string `json:"version"`
		Runs    []struct {
			Tool struct {
				Driver struct {
					Name  string `json:"name"`
					Rules []struct {
						ID                   string `json:"id"`
						DefaultConfiguration struct {
							Level string `json:"level"`
						} `json:"defaultConfiguration"`
					} `json:"rules"`
				} `json:"driver"`
			} `json:"tool"`
			Results []struct {
				RuleID    string `json:"ruleId"`
				Level     string `json:"level"`
				Locations []struct {
					PhysicalLocation struct {
						ArtifactLocation struct {
							URI string `json:"uri"`
						} `json:"artifactLocation"`
						Region struct {
							StartLine   int `json:"startLine"`
							StartColumn int `json:"startColumn"`
						} `json:"region"`
					} `json:"physicalLocation"`
				} `json:"locations"`
			} `json:"results"`
		} `json:"runs"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &doc), "output must be valid SARIF JSON:\n%s", buf.String())

	assert.Equal(t, "2.1.0", doc.Version)
	require.Len(t, doc.Runs, 1)
	assert.Equal(t, "mache find-smells", doc.Runs[0].Tool.Driver.Name)

	require.Len(t, doc.Runs[0].Tool.Driver.Rules, 1)
	assert.Equal(t, "dead_code", doc.Runs[0].Tool.Driver.Rules[0].ID)
	assert.Equal(t, "warning", doc.Runs[0].Tool.Driver.Rules[0].DefaultConfiguration.Level)

	require.Len(t, doc.Runs[0].Results, 1)
	r := doc.Runs[0].Results[0]
	assert.Equal(t, "dead_code", r.RuleID)
	assert.Equal(t, "warning", r.Level)
	require.Len(t, r.Locations, 1)
	assert.NotEmpty(t, r.Locations[0].PhysicalLocation.ArtifactLocation.URI)
	assert.GreaterOrEqual(t, r.Locations[0].PhysicalLocation.Region.StartLine, 1)
	assert.GreaterOrEqual(t, r.Locations[0].PhysicalLocation.Region.StartColumn, 1)
}
