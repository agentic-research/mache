package examples_test

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/lltest"
	"github.com/agentic-research/mache/internal/mcpharness"
	"github.com/agentic-research/mache/internal/smells"
	machetmpl "github.com/agentic-research/mache/internal/template"
	"github.com/agentic-research/mache/internal/testutil"
	publicschema "github.com/agentic-research/mache/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"
)

// projector builds the graph a schema projects from its sample, through the
// path the documented command takes for that kind of source.
type projector func(t *testing.T, topo *api.Topology) graph.Graph

// exampleCase is one schema in this directory and the sample it is rendered
// against.
type exampleCase struct {
	project projector
	// want are paths that must exist in the projection: the sample's records
	// landed where the README says they do.
	want []string
	// nullable are leaf names whose source field is optional in the data
	// contract. They may render empty; the sample carries a record where the
	// field is absent, so a nil reaching the template still shows up as
	// `<no value>`.
	nullable []string
}

// exampleCases is every *-schema.json in this directory.
// TestEveryExampleHasACase fails when a schema is added without one.
var exampleCases = map[string]exampleCase{
	// Venturi data: records wrapped as {schema, identifier, item}.
	"kev-schema.json": {
		project: venturiDB("testdata/kev.json"),
		want:    []string{"vulns/CVE-2021-44228/vendor", "vulns/CVE-2023-4966/due-date"},
	},
	"nvd-schema.json": {
		project: venturiDB("testdata/nvd.json"),
		want:    []string{"by-cve/2024/03/CVE-2024-3094/description", "by-cve/2024/07/CVE-2024-6387/status"},
	},
	"notion-schema.json": {
		project: venturiDB("testdata/notion.json"),
		want:    []string{"pages/Roadmap/properties.json", "pages/Onboarding/raw.json"},
	},
	"notion-repos-schema.json": {
		project: venturiDB("testdata/notion-repos.json"),
		want:    []string{"repos/projection/mache/purpose", "repos/substrate/ley-line/path"},
	},
	"mcp-registry-schema.json": {
		project: venturiDB("testdata/mcp-registry.json"),
		want:    []string{"servers/io.github.example/weather/version", "servers/io.github.example/notes/repository"},
	},
	"trivy-ghsa-schema.json": {
		project: venturiEngine("testdata/trivy-ghsa.json"),
		want: []string{
			"advisories/Maven::ghsa/org.apache.logging.log4j:log4j-core/CVE-2021-44228",
			"advisories/Npm::ghsa/left-pad/GHSA-xxxx-yyyy-zzzz",
		},
	},
	// Plain JSON documents.
	"llm-conversations-schema.json": {
		project:  jsonFile("llm-conversations-sample.json"),
		want:     []string{"anthropic/2025-06/claude-sonnet-4-20250514/conv_01JZ3X9K2M/transcript", "openai/2025-06/o3/conv_05ND7B3O6R/token-usage"},
		nullable: []string{"system-prompt"},
	},
	"mcp-schema.json": {
		project: jsonFile("mcp-sample-manifest.json"),
		want: []string{
			"tools/search-issues/input-schema.json",
			"resources/repository-readme/uri",
			"prompts/summarize-repo/description",
		},
	},
	"audit-schema.json": {
		project:  jsonFile("test-audit/.index.json"),
		want:     []string{"findings/by-severity/critical/C1/title", "scores/security/rubric", "timeline/cycle-1/timestamp"},
		nullable: []string{"owned_files", "notes"},
	},
	"bdr-schema.json": {
		project:  jsonFile("testdata/beads.json"),
		want:     []string{"by-status/open/demo-a1b2c3/title", "by-repo/demo/demo-d4e5f6/pr_url", "threads/gh-12/demo-d4e5f6/title"},
		nullable: []string{"owner", "external_ref", "branch", "pr_url"},
	},
	"bdr-hierarchy-schema.json": {
		project: jsonFile("testdata/beads.json"),
	},
	// Source code, parsed by the pinned leyline.
	"go-schema.json": {
		project: leylineSource("testdata/go_sample.go"),
		want:    []string{"main/functions/Main/source", "main/functions/Helper/source"},
	},
	"python-schema.json": {
		project: leylineSource("testdata/python_sample.py"),
		want:    []string{"imports/os/source", "classes/MyClass/source", "functions/hello/source"},
	},
	"sql-schema.json": {
		project: leylineSource("testdata/sql_sample.sql"),
		want:    []string{"tables/users/source", "views/user_names/source"},
	},
	"cli-schema.json": {
		project: leylineSource("testdata/cli_sample.go"),
		want:    []string{"main/commands/rootCmd/Use/value", "main/flags/toggle/info"},
	},
	"rust-schema.json": {
		project: presetFixture("rust"),
		want:    []string{"functions/free/source", "methods/Grid.new/source"},
	},
	"terraform-schema.json": {
		project: presetFixture("terraform"),
		want:    []string{`resources/"aws_s3_bucket"/source`, `variables/"region"/source`},
	},
	"markdown-schema.json": {
		project: presetFixture("markdown"),
		want:    []string{"sections/Code/source", "code/go/source", "code/sh/source"},
	},
}

// knownBroken are schemas that do not project cleanly today, each with the
// bead that tracks it. TestExampleSchemasProject asserts they are STILL
// broken, so the fix has to remove the entry.
var knownBroken = map[string]string{
	"bdr-hierarchy-schema.json": "mache-fabfec: reads source_adr/channel/thread_group and a per-decade .beads aggregate that no producer emits",
}

// notMirrors are examples named after a preset that deliberately differ
// from it, with why.
var notMirrors = map[string]string{
	"sql-schema.json":      "mache-fa8b02: the preset's functions/types/indexes are named {{.index}}, which no walker provides",
	"markdown-schema.json": "mache-fa8b02: every preset node but sections is named {{.index}}, which no walker provides",
}

// loadExample reads a schema the way `mache --schema` does: parse, then
// expand file_set includes.
func loadExample(t *testing.T, name string) *api.Topology {
	t.Helper()
	res, err := publicschema.Resolve(name, ".")
	require.NoError(t, err)
	res.Topology.ResolveIncludes()
	return res.Topology
}

// jsonFile projects a JSON document through the engine, as `mache serve
// --schema S data.json` does.
func jsonFile(sample string) projector {
	return func(t *testing.T, topo *api.Topology) graph.Graph {
		store := graph.NewMemoryStore()
		abs, err := filepath.Abs(sample)
		require.NoError(t, err)
		require.NoError(t, ingest.NewEngine(topo, store).Ingest(abs))
		return store
	}
}

// venturiResults writes sample, a JSON array of Venturi records, into the
// results(id, record) table Venturi and tools/mcp-fetch produce.
func venturiResults(t *testing.T, sample string) string {
	t.Helper()
	raw, err := os.ReadFile(sample)
	require.NoError(t, err)
	var records []map[string]any
	require.NoError(t, json.Unmarshal(raw, &records))
	require.NotEmpty(t, records)

	dbPath := filepath.Join(t.TempDir(), "data.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	_, err = db.Exec(`CREATE TABLE results (id TEXT PRIMARY KEY, record TEXT NOT NULL)`)
	require.NoError(t, err)
	for _, rec := range records {
		body, err := json.Marshal(rec)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO results (id, record) VALUES (?, ?)`, rec["identifier"], string(body))
		require.NoError(t, err)
	}
	return dbPath
}

// venturiDB serves a Venturi results db over the direct SQL backend, as
// `mache serve --schema S data.db` and the mount do.
func venturiDB(sample string) projector {
	return func(t *testing.T, topo *api.Topology) graph.Graph {
		sg, err := graph.OpenSQLiteGraph(venturiResults(t, sample), topo, machetmpl.Render)
		require.NoError(t, err)
		t.Cleanup(func() { _ = sg.Close() })
		return sg
	}
}

// venturiEngine streams a Venturi results db through the engine, the path
// `mache --data data.db --out` takes (trivy-ghsa's documented use).
func venturiEngine(sample string) projector {
	return func(t *testing.T, topo *api.Topology) graph.Graph {
		store := graph.NewMemoryStore()
		require.NoError(t, ingest.NewEngine(topo, store).Ingest(venturiResults(t, sample)))
		return store
	}
}

// leylineSource parses a source sample with the pinned leyline and projects
// it, as `mache build --schema S` does.
func leylineSource(sample string) projector {
	return func(t *testing.T, topo *api.Topology) graph.Graph {
		abs, err := filepath.Abs(sample)
		require.NoError(t, err)
		return leylineProject(t, topo, abs)
	}
}

// presetFixture projects the preset fixture for lang: the sources the preset
// of the same name is tested against.
func presetFixture(lang string) projector {
	return func(t *testing.T, topo *api.Topology) graph.Graph {
		return leylineProject(t, topo, filepath.Join(testutil.PresetFixturesDir(t), lang))
	}
}

func leylineProject(t *testing.T, topo *api.Topology, src string) graph.Graph {
	t.Helper()
	mcpharness.IsolatedHome(t)
	store := graph.NewMemoryStore()
	lltest.IngestSourceViaLeyline(t, ingest.NewEngine(topo, store), src)
	return store
}

// walkProjection visits every node under the root, as a reader of the mount
// would, and returns the leaf paths it read and what it found wrong: a node
// named from a field the record does not have (`<no value>`), a leaf whose
// content contains one, and a leaf that renders empty.
func walkProjection(t *testing.T, g graph.Graph, nullable []string) (leaves, problems []string) {
	t.Helper()
	var walk func(id string)
	walk = func(id string) {
		children, err := g.ListChildren(id)
		require.NoError(t, err, "list %q", id)
		for _, child := range children {
			name := filepath.Base(child)
			if strings.Contains(name, "<no value>") {
				problems = append(problems, child+": named from a missing field")
			}
			node, err := g.GetNode(child)
			require.NoError(t, err, "get %q", child)
			if node.Mode.IsDir() {
				walk(child)
				continue
			}
			leaves = append(leaves, child)
			buf := make([]byte, node.ContentSize())
			n, err := g.ReadContent(child, buf, 0)
			require.NoError(t, err, "read %q", child)
			content := string(buf[:n])
			switch {
			case strings.Contains(content, "<no value>"):
				problems = append(problems, child+": content rendered from a missing field")
			case strings.TrimSpace(content) == "" && !slices.Contains(nullable, name):
				problems = append(problems, child+": renders empty")
			}
		}
	}
	walk("")
	return leaves, problems
}

// TestEveryExampleHasACase: a schema in this directory that no case renders
// is a schema nothing checks.
func TestEveryExampleHasACase(t *testing.T) {
	schemas, err := filepath.Glob("*-schema.json")
	require.NoError(t, err)
	require.NotEmpty(t, schemas)
	for _, sf := range schemas {
		assert.Contains(t, exampleCases, sf, "%s has no case in exampleCases", sf)
	}
	for sf := range exampleCases {
		assert.FileExists(t, sf)
	}
	for sf := range knownBroken {
		assert.Contains(t, exampleCases, sf)
	}
}

// TestExampleSchemasProject renders every schema against its sample and walks
// the whole projection.
func TestExampleSchemasProject(t *testing.T) {
	for sf, tc := range exampleCases {
		t.Run(sf, func(t *testing.T) {
			topo := loadExample(t, sf)
			assert.Equal(t, api.SchemaVersion, topo.Version)
			g := tc.project(t, topo)
			leaves, problems := walkProjection(t, g, tc.nullable)
			if bead, broken := knownBroken[sf]; broken {
				assert.NotEmpty(t, problems, "%s now projects cleanly: remove it from knownBroken (%s)", sf, bead)
				return
			}
			assert.Empty(t, problems)
			require.NotEmpty(t, leaves, "the sample projected no files")
			for _, p := range tc.want {
				_, err := g.GetNode(p)
				assert.NoError(t, err, "node %s not found", p)
			}
		})
	}
}

// TestMCPInputSchemaIsJSON: the mcp example's point is the per-tool
// input-schema.json, so it has to be the tool's JSON Schema, not just bytes.
func TestMCPInputSchemaIsJSON(t *testing.T) {
	g := jsonFile("mcp-sample-manifest.json")(t, loadExample(t, "mcp-schema.json"))
	node, err := g.GetNode("tools/search-issues/input-schema.json")
	require.NoError(t, err)
	buf := make([]byte, node.ContentSize())
	n, err := g.ReadContent(node.ID, buf, 0)
	require.NoError(t, err)
	var inputSchema map[string]any
	require.NoError(t, json.Unmarshal(buf[:n], &inputSchema))
	assert.Equal(t, "object", inputSchema["type"])
}

// TestExamplesMirrorPresets: an example named after a preset is a copy of it
// (`--schema go` and `--schema examples/go-schema.json` project the same
// tree), unless notMirrors says why not.
func TestExamplesMirrorPresets(t *testing.T) {
	mirrored := 0
	for _, name := range publicschema.AvailablePresets() {
		sf := name + "-schema.json"
		if _, err := os.Stat(sf); err != nil {
			continue
		}
		t.Run(sf, func(t *testing.T) {
			preset, err := publicschema.LoadPreset(name)
			require.NoError(t, err)
			example := loadExample(t, sf)
			preset.Topology.ResolveIncludes()
			if why, differs := notMirrors[sf]; differs {
				assert.NotEqual(t, preset.Topology, example, "%s now equals the preset: remove it from notMirrors (%s)", sf, why)
				return
			}
			mirrored++
			assert.Equal(t, preset.Topology, example, "%s has drifted from schema/presets/%s.json", sf, name)
		})
	}
	assert.Positive(t, mirrored)
	for sf := range notMirrors {
		assert.FileExists(t, sf)
	}
}

// smellCorpus is the source a kit rule runs against: a small Go package with
// one construct each rule must flag, and MintCanonical, which
// uncanonicalized_path_identity must not.
var smellCorpus = map[string]string{
	"lib.go": "package lib\n\nimport (\n\t\"log\"\n\t\"path/filepath\"\n)\n\n" +
		"func projectToken(p string) string { return p }\n\n" +
		"func Mint(p string) string {\n\treturn projectToken(p)\n}\n\n" +
		"func MintCanonical(p string) string {\n\tc, _ := filepath.EvalSymlinks(p)\n\treturn projectToken(c)\n}\n\n" +
		"func Must(err error) {\n\tif err != nil {\n\t\tlog.Fatal(err)\n\t}\n}\n\n" +
		"func longHelper() int {\n\tx := 0\n" + strings.Repeat("\tx++\n", 205) + "\treturn x\n}\n",
	"lib_test.go": "package lib\n\nimport \"testing\"\n\n" +
		"func TestLong(t *testing.T) {\n\tx := 0\n" + strings.Repeat("\tx++\n", 125) + "\t_ = x\n}\n",
}

// lineOf is the 1-based line of the first occurrence of text in file.
func lineOf(t *testing.T, file, text string) int {
	t.Helper()
	src := smellCorpus[file]
	i := strings.Index(src, text)
	require.GreaterOrEqual(t, i, 0, "%q not in %s", text, file)
	return strings.Count(src[:i], "\n") + 1
}

// TestSmellRulesKit runs each rule in smell-rules/ the way its README does,
// `mache find-smells --rules-dir smell-rules --rule ID`, on a leyline build of
// smellCorpus, and checks it reports exactly the construct it is written for,
// at its file and line.
func TestSmellRulesKit(t *testing.T) {
	mcpharness.IsolatedHome(t)
	dir := t.TempDir()
	for name, src := range smellCorpus {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644))
	}
	db, _ := lltest.ParseSourceViaLeyline(t, dir)
	var dbPath string
	require.NoError(t, db.QueryRow(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&dbPath))
	rulesDir, err := filepath.Abs("smell-rules")
	require.NoError(t, err)

	type site struct{ file, at string }
	want := map[string]site{
		"fatal_call_in_library":         {"lib.go", "log.Fatal(err)"},
		"long_test_function":            {"lib_test.go", "func TestLong("},
		"long_unexported_function":      {"lib.go", "func longHelper("},
		"uncanonicalized_path_identity": {"lib.go", "func Mint("},
	}
	rules, err := filepath.Glob(filepath.Join(rulesDir, "*.json"))
	require.NoError(t, err)
	require.Len(t, rules, len(want), "every rule in smell-rules/ needs an expected finding here")

	for id, w := range want {
		t.Run(id, func(t *testing.T) {
			var out, errw strings.Builder
			code, err := smells.RunFindSmells(smells.FindSmellsOptions{
				DB: dbPath, Rule: id, RulesDir: rulesDir, FailOn: "none", Out: &out, Err: &errw,
			})
			require.NoError(t, err, errw.String())
			require.Zero(t, code, "find-smells failed: %s", errw.String())
			var resp struct {
				Findings []struct {
					SourceID string `json:"source_id"`
					Line     int    `json:"line"`
				} `json:"findings"`
			}
			require.NoError(t, json.Unmarshal([]byte(out.String()), &resp), out.String())
			require.Len(t, resp.Findings, 1, out.String())
			assert.Equal(t, w.file, resp.Findings[0].SourceID)
			assert.Equal(t, lineOf(t, w.file, w.at), resp.Findings[0].Line, out.String())
		})
	}
}
