package build

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedCoverageFixture(t *testing.T) (*sql.DB, string) {
	t.Helper()
	_, fixture := fixturedb.New(t, fixturedb.Leyline).
		ASTNode("main.go/function_declaration", "function_declaration", "main.go", fixturedb.Bytes(0, 14)).
		Build()

	source := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, "main.go"),
		[]byte("package main\nfunc main() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(source, "util.sql"),
		[]byte("CREATE TABLE users (id INTEGER);\n"), 0o644))
	return fixture.DB(), source
}

func TestSchemaCoverageGaps(t *testing.T) {
	db, source := seedCoverageFixture(t)
	topologyFor := func(languages ...string) *api.Topology {
		topology := &api.Topology{Version: "v1"}
		for _, language := range languages {
			topology.Nodes = append(topology.Nodes, api.Node{Name: language, Language: language})
		}
		return topology
	}

	gaps, err := schemaCoverageGaps(db, topologyFor("go", "sql"), source, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"sql"}, gaps)

	gaps, err = schemaCoverageGaps(db, topologyFor("go", "rust"), source, nil)
	require.NoError(t, err)
	assert.Empty(t, gaps)

	nested := &api.Topology{Version: "v1", Nodes: []api.Node{
		{Name: "root", Children: []api.Node{{Name: "tables", Language: "sql"}}},
	}}
	gaps, err = schemaCoverageGaps(db, nested, source, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"sql"}, gaps)

	hintless := &api.Topology{Version: "v1", Nodes: []api.Node{{Name: "tables"}}}
	gaps, err = schemaCoverageGaps(db, hintless, source, nil)
	require.NoError(t, err)
	assert.Nil(t, gaps)

	gaps, err = schemaCoverageGaps(db, hintless, source, []string{"sql"})
	require.NoError(t, err)
	assert.Equal(t, []string{"sql"}, gaps)

	gaps, err = schemaCoverageGaps(db, topologyFor("klingon"), source, nil)
	require.NoError(t, err)
	assert.Empty(t, gaps)
}
