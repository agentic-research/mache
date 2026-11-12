package reference_test

// schema.md is held to api/schema.go two ways.
//
// TestSchemaReferenceDocumentsEveryField walks the struct types reachable
// from api.Topology by reflection and requires a "### `<json tag>`" heading
// for each json field under a "## <TypeName>" section, and no heading for a
// field that does not exist. Adding a field to api/schema.go without a
// section here fails the test.
//
// TestSchemaReferenceSnippetsProject projects every "```json schema:<kind>"
// fence over the "sample:<kind>" fence of the same kind, with the same
// machinery examples/examples_test.go uses (internal/projtest), and requires
// the "```text tree" fence that follows to list exactly the paths the
// projection has, with no `<no value>` and no empty leaf.
//
// TestSchemaReferenceClaims pins the behaviours the prose asserts that no
// snippet can show (a field ignored, a path dropped), so the sentence has to
// change when the code does.

import (
	"bufio"
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/projtest"
	"github.com/agentic-research/mache/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const docPath = "schema.md"

// fence is one fenced code block: the info string after the backticks and
// the lines between the fences.
type fence struct {
	info string
	body string
	line int // 1-based line of the opening fence
}

// document is schema.md read structurally: its fences in order, and the
// "### `field`" headings grouped by the "## Section" they sit under.
type document struct {
	fences   []fence
	headings map[string][]string // section → documented field names
}

// readDocument scans schema.md line by line. A line starting with ``` opens
// or closes a fence; outside fences, "## " sets the section and "### `x`"
// records x under it.
func readDocument(t *testing.T) document {
	t.Helper()
	f, err := os.Open(docPath)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	doc := document{headings: map[string][]string{}}
	var (
		open    *fence
		body    []string
		section string
		lineNo  int
	)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if strings.HasPrefix(line, "```") {
			if open == nil {
				open = &fence{info: strings.TrimSpace(strings.TrimPrefix(line, "```")), line: lineNo}
				body = nil
				continue
			}
			open.body = strings.Join(body, "\n")
			doc.fences = append(doc.fences, *open)
			open = nil
			continue
		}
		if open != nil {
			body = append(body, line)
			continue
		}
		if rest, ok := strings.CutPrefix(line, "## "); ok {
			section = strings.TrimSpace(rest)
			continue
		}
		if rest, ok := strings.CutPrefix(line, "### `"); ok {
			name, ok := strings.CutSuffix(rest, "`")
			if ok {
				doc.headings[section] = append(doc.headings[section], name)
			}
		}
	}
	require.NoError(t, sc.Err())
	require.Nil(t, open, "unclosed fence opened at line %d", lineNo)
	return doc
}

// jsonFields walks the struct types reachable from root through fields,
// pointers, slices and maps, and returns type name → json field names for
// every struct in root's package.
func jsonFields(root reflect.Type) map[string][]string {
	out := map[string][]string{}
	var visit func(t reflect.Type)
	visit = func(t reflect.Type) {
		for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Map {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct || t.PkgPath() != root.PkgPath() {
			return
		}
		if _, seen := out[t.Name()]; seen {
			return
		}
		out[t.Name()] = nil
		for i := range t.NumField() {
			f := t.Field(i)
			tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if tag == "" || tag == "-" {
				continue
			}
			out[t.Name()] = append(out[t.Name()], tag)
			visit(f.Type)
		}
	}
	visit(root)
	return out
}

func TestSchemaReferenceDocumentsEveryField(t *testing.T) {
	doc := readDocument(t)
	want := jsonFields(reflect.TypeOf(api.Topology{}))
	require.Contains(t, want, "Topology")
	require.Contains(t, want, "Leaf")

	for typeName, fields := range want {
		got := doc.headings[typeName]
		sort.Strings(fields)
		sort.Strings(got)
		assert.Equal(t, fields, got,
			"## %s in %s must have one ### `field` heading per json field of api.%s, and no other", typeName, docPath, typeName)
	}
	for section := range doc.headings {
		assert.Contains(t, want, section,
			"## %s has field headings but api has no type of that name", section)
	}
}

// samples are the inputs the snippets project over, keyed by kind.
type samples struct {
	data    []byte            // the JSON document
	db      []byte            // JSON array of Venturi records
	goFiles map[string]string // file name → Go source
}

func collectSamples(t *testing.T, doc document) samples {
	t.Helper()
	s := samples{goFiles: map[string]string{}}
	for _, f := range doc.fences {
		lang, rest, _ := strings.Cut(f.info, " ")
		kind, arg, _ := strings.Cut(rest, " ")
		switch kind {
		case "sample:data":
			require.Equal(t, "json", lang)
			require.Nil(t, s.data, "two sample:data fences")
			s.data = []byte(f.body)
		case "sample:db":
			require.Equal(t, "json", lang)
			require.Nil(t, s.db, "two sample:db fences")
			s.db = []byte(f.body)
		case "sample:go":
			require.Equal(t, "go", lang)
			require.NotEmpty(t, arg, "sample:go needs a file name (line %d)", f.line)
			s.goFiles[arg] = f.body
		}
	}
	require.NotNil(t, s.data, "no sample:data fence")
	require.NotNil(t, s.db, "no sample:db fence")
	require.NotEmpty(t, s.goFiles, "no sample:go fence")
	require.True(t, json.Valid(s.data))
	require.True(t, json.Valid(s.db))
	return s
}

// project renders topo over the sample for kind, the way the documented
// command for that kind does.
func (s samples) project(t *testing.T, kind string, topo *api.Topology) graph.Graph {
	t.Helper()
	switch kind {
	case "data":
		dir := projtest.WriteFiles(t, map[string]string{"doc.json": string(s.data)})
		return projtest.JSONDocument(t, topo, dir+"/doc.json")
	case "db":
		return projtest.VenturiDirect(t, topo, projtest.VenturiResults(t, s.db, topo.Table))
	case "go":
		return projtest.LeylineSource(t, topo, projtest.WriteFiles(t, s.goFiles))
	}
	t.Fatalf("unknown snippet kind %q", kind)
	return nil
}

// treeLines parses a "text tree" fence: one path per line, optionally
// followed by " = content" asserting the leaf's trimmed content.
func treeLines(body string) (paths []string, contents map[string]string) {
	contents = map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		path, content, has := strings.Cut(line, " = ")
		paths = append(paths, path)
		if has {
			contents[path] = content
		}
	}
	sort.Strings(paths)
	return paths, contents
}

func TestSchemaReferenceSnippetsProject(t *testing.T) {
	doc := readDocument(t)
	s := collectSamples(t, doc)

	snippets := 0
	for i, f := range doc.fences {
		lang, rest, _ := strings.Cut(f.info, " ")
		if lang != "json" {
			continue
		}
		kind, isSchema := strings.CutPrefix(rest, "schema:")
		if !isSchema {
			var topo api.Topology
			if json.Unmarshal([]byte(f.body), &topo) == nil && len(topo.Nodes) > 0 {
				t.Errorf("line %d: a topology in a ```json fence without a schema:<kind> tag is not checked", f.line)
			}
			continue
		}
		snippets++
		t.Run(f.info+"@"+strconv.Itoa(f.line), func(t *testing.T) {
			require.Less(t, i+1, len(doc.fences), "schema snippet needs a ```text tree fence after it")
			tree := doc.fences[i+1]
			require.Equal(t, "text tree", tree.info, "the fence after a schema snippet must be ```text tree (line %d)", tree.line)

			topo, err := schema.ParseTopology([]byte(f.body))
			require.NoError(t, err)
			topo.ResolveIncludes()

			g := s.project(t, kind, topo)
			dirs, leaves, problems := projtest.Walk(t, g, nil)
			assert.Empty(t, problems)
			require.NotEmpty(t, leaves, "the snippet projected no files")

			wantPaths, wantContent := treeLines(tree.body)
			got := slices.Concat(dirs, leaves)
			sort.Strings(got)
			assert.Equal(t, wantPaths, got, "the tree fence at line %d must list exactly the projected paths", tree.line)
			for path, want := range wantContent {
				node, err := g.GetNode(path)
				require.NoError(t, err, "tree lists %s with content", path)
				assert.Equal(t, want, strings.TrimSpace(projtest.Read(t, g, node)), "content of %s", path)
			}
		})
	}
	require.Positive(t, snippets, "no schema:<kind> snippets found in %s", docPath)
}

// fixtures is what every claim runs against: the doc's samples, a results db
// built from the db sample, and the data sample on disk.
type fixtures struct {
	s        samples
	dbPath   string
	dataFile string
}

// topology parses a one-line schema or fails the test.
func topology(t *testing.T, src string) *api.Topology {
	t.Helper()
	topo, err := schema.ParseTopology([]byte(src))
	require.NoError(t, err)
	return topo
}

// claims are the sentences in schema.md that describe a field doing nothing
// or a path diverging. Each names the behaviour it backs; when one fails,
// the code changed and the section must be rewritten (and the bead the
// section names closed).
var claims = map[string]func(t *testing.T, fx fixtures){
	"version is not checked": func(t *testing.T, fx fixtures) {
		topo := topology(t, `{"version":"v9","nodes":[{"name":"x","selector":"$","files":[{"name":"f","content_template":"ok"}]}]}`)
		_, leaves, _ := projtest.Walk(t, projtest.JSONDocument(t, topo, fx.dataFile), nil)
		assert.Equal(t, []string{"x/f"}, leaves)
	},
	"attributes.mode is not applied": func(t *testing.T, fx fixtures) {
		topo := topology(t, `{"version":"v1","nodes":[{"name":"x","selector":"$","files":[{"name":"f","content_template":"ok","attributes":{"mode":420}}]}]}`)
		require.EqualValues(t, 420, topo.Nodes[0].Files[0].Attributes.Mode)
		node, err := projtest.JSONDocument(t, topo, fx.dataFile).GetNode("x/f")
		require.NoError(t, err)
		assert.EqualValues(t, 0o444, node.Mode.Perm())
	},
	"table is honoured by the direct path and ignored by the engine .db path": func(t *testing.T, fx fixtures) {
		topo := topology(t, `{"version":"v1","table":"advisories","nodes":[{"name":"vulns","children":[{"name":"{{.item.cveID}}","selector":"$[*]","files":[{"name":"vendor","content_template":"{{.item.vendorProject}}"}]}]}]}`)
		named := projtest.VenturiResults(t, fx.s.db, "advisories")
		_, leaves, problems := projtest.Walk(t, projtest.VenturiDirect(t, topo, named), nil)
		assert.Empty(t, problems)
		assert.Equal(t, []string{"vulns/CVE-2021-44228/vendor", "vulns/CVE-2023-4966/vendor"}, leaves)

		err := ingest.NewEngine(topo, graph.NewMemoryStore()).Ingest(named)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no such table: results")
	},
	"direct path ignores selector": func(t *testing.T, fx fixtures) {
		topo := topology(t, `{"version":"v1","nodes":[{"name":"vulns","selector":"$.nothing[*]","children":[{"name":"{{.item.cveID}}","selector":"$.nothing[*]","files":[{"name":"vendor","content_template":"{{.item.vendorProject}}"}]}]}]}`)
		_, leaves, _ := projtest.Walk(t, projtest.VenturiDirect(t, topo, fx.dbPath), nil)
		assert.Equal(t, []string{"vulns/CVE-2021-44228/vendor", "vulns/CVE-2023-4966/vendor"}, leaves)
	},
	"direct path does not render leaf names": func(t *testing.T, fx fixtures) {
		topo := topology(t, `{"version":"v1","nodes":[{"name":"vulns","children":[{"name":"{{.item.cveID}}","files":[{"name":"{{.item.product}}","content_template":"{{.item.vendorProject}}"}]}]}]}`)
		g := projtest.VenturiDirect(t, topo, fx.dbPath)
		children, err := g.ListChildren("vulns/CVE-2021-44228")
		require.NoError(t, err)
		assert.Equal(t, []string{"vulns/CVE-2021-44228/{{.item.product}}"}, children)
		_, err = g.GetNode("vulns/CVE-2021-44228/Log4j")
		assert.ErrorIs(t, err, graph.ErrNotFound)
	},
	"direct path does not inject _parent": func(t *testing.T, fx fixtures) {
		topo := topology(t, `{"version":"v1","nodes":[{"name":"vulns","children":[{"name":"{{.item.cveID}}","children":[{"name":"{{._parent.item.cveID}}","files":[{"name":"vendor","content_template":"{{.item.vendorProject}}"}]}]}]}]}`)
		_, leaves, _ := projtest.Walk(t, projtest.VenturiDirect(t, topo, fx.dbPath), nil)
		assert.Empty(t, leaves, "every record is null-skipped")
	},
	"engine drops empty leaves except source": func(t *testing.T, fx fixtures) {
		topo := topology(t, `{"version":"v1","nodes":[{"name":"x","selector":"$","files":[{"name":"empty","content_template":"{{default .missing \"\"}}"},{"name":"source","content_template":""}]}]}`)
		_, leaves, problems := projtest.Walk(t, projtest.JSONDocument(t, topo, fx.dataFile), []string{"source"})
		assert.Empty(t, problems)
		assert.Equal(t, []string{"x/source"}, leaves)
	},
	"engine .db path keeps empty leaves and ignores the root selector": func(t *testing.T, fx fixtures) {
		topo := topology(t, `{"version":"v1","nodes":[{"name":"vulns","selector":"$.nothing","children":[{"name":"{{.item.cveID}}","selector":"$[*]","files":[{"name":"empty","content_template":"{{default .item.missing \"\"}}"}]}]}]}`)
		_, leaves, _ := projtest.Walk(t, projtest.VenturiEngine(t, topo, fx.dbPath), []string{"empty"})
		assert.Equal(t, []string{"vulns/CVE-2021-44228/empty", "vulns/CVE-2023-4966/empty"}, leaves)
	},
	"an empty JSONPath selector matches nothing": func(t *testing.T, fx fixtures) {
		topo := topology(t, `{"version":"v1","nodes":[{"name":"x","selector":"","files":[{"name":"f","content_template":"ok"}]}]}`)
		g := projtest.JSONDocument(t, topo, fx.dataFile)
		roots, err := g.ListChildren("")
		require.NoError(t, err)
		assert.Empty(t, roots)
	},
	"recursive descent and non-object matches expose .value": func(t *testing.T, fx fixtures) {
		topo := topology(t, `{"version":"v1","nodes":[{"name":"names","selector":"$","children":[{"name":"{{.value}}","selector":"$..name","files":[{"name":"v","content_template":"{{.value}}"}]}]}]}`)
		_, leaves, _ := projtest.Walk(t, projtest.JSONDocument(t, topo, fx.dataFile), nil)
		assert.Equal(t, []string{"names/alerts/v", "names/forecast/v", "names/notes/v", "names/search/v", "names/weather/v"}, leaves)
	},
	"refs feed find_callers on the engine path": func(t *testing.T, fx fixtures) {
		topo := topology(t, `{"version":"v1","nodes":[{"name":"tools","selector":"$","children":[{"name":"{{.name}}","selector":"$.servers[*].tools[*]","refs":["{{.name}}"],"files":[{"name":"d","content_template":"{{.description}}"}]}]}]}`)
		callers, err := projtest.JSONDocument(t, topo, fx.dataFile).GetCallers("forecast")
		require.NoError(t, err)
		require.Len(t, callers, 1)
		assert.Equal(t, "tools/forecast", callers[0].ID)
	},
	"skip_self_match does not act on JSON": func(t *testing.T, fx fixtures) {
		topo := topology(t, `{"version":"v1","nodes":[{"name":"s","selector":"$","children":[{"name":"{{.name}}","selector":"$.servers[*]","skip_self_match":true,"files":[{"name":"v","content_template":"{{.version}}"}]}]}]}`)
		_, leaves, _ := projtest.Walk(t, projtest.JSONDocument(t, topo, fx.dataFile), nil)
		assert.Equal(t, []string{"s/notes/v", "s/weather/v"}, leaves)
	},
	"include of an unknown file set is ignored and a second expansion appends again": func(t *testing.T, _ fixtures) {
		topo := topology(t, `{"version":"v1","file_sets":{"a":[{"name":"n","content_template":"{{.name}}"}]},"nodes":[{"name":"{{.name}}","selector":"$.servers[*]","include":["missing","a"],"files":[{"name":"v","content_template":"{{.version}}"}]}]}`)
		topo.ResolveIncludes()
		require.Len(t, topo.Nodes[0].Files, 2)
		assert.Equal(t, "n", topo.Nodes[0].Files[1].Name)
		topo.ResolveIncludes()
		assert.Len(t, topo.Nodes[0].Files, 3)
	},
}

func TestSchemaReferenceClaims(t *testing.T) {
	doc := readDocument(t)
	s := collectSamples(t, doc)
	fx := fixtures{
		s:        s,
		dbPath:   projtest.VenturiResults(t, s.db, ""),
		dataFile: projtest.WriteFiles(t, map[string]string{"doc.json": string(s.data)}) + "/doc.json",
	}
	for name, run := range claims {
		t.Run(name, func(t *testing.T) { run(t, fx) })
	}
}
