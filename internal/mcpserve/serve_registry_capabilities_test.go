package mcpserve

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/graph"
)

// capabilityInterfaces is every optional capability graph/capabilities.go
// declares. Handlers reach them by type assertion on the graph they are given,
// which in serve mode is a *lazyGraph, so a capability the lazyGraph does not
// forward is silently absent there: the handler takes its fallback. That is
// how DBPath went missing once, and RefTokenCount (mache-a6c283) after it,
// whose fallback made the live daemon's get_overview 4x slower than the same
// code in process.
var capabilityInterfaces = map[string]reflect.Type{
	"DefsLookuper":     reflect.TypeFor[graph.DefsLookuper](),
	"DefsSearcher":     reflect.TypeFor[graph.DefsSearcher](),
	"DefsMapper":       reflect.TypeFor[graph.DefsMapper](),
	"RefsMapper":       reflect.TypeFor[graph.RefsMapper](),
	"RefTokenCounter":  reflect.TypeFor[graph.RefTokenCounter](),
	"RefsQuerier":      reflect.TypeFor[graph.RefsQuerier](),
	"DBPathProvider":   reflect.TypeFor[graph.DBPathProvider](),
	"MountPrefixer":    reflect.TypeFor[graph.MountPrefixer](),
	"RefResolver":      reflect.TypeFor[graph.RefResolver](),
	"ASTKinder":        reflect.TypeFor[graph.ASTKinder](),
	"DefTokener":       reflect.TypeFor[graph.DefTokener](),
	"DefsNodeLookuper": reflect.TypeFor[graph.DefsNodeLookuper](),
}

// The list above is every interface capabilities.go declares, so one added
// there cannot be skipped here.
func TestCapabilityInterfaces_ListsEveryDeclaredCapability(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "../../graph/capabilities.go", nil, 0)
	require.NoError(t, err)
	var declared []string
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, s := range gd.Specs {
			if ts, ok := s.(*ast.TypeSpec); ok {
				if _, isIface := ts.Type.(*ast.InterfaceType); isIface {
					declared = append(declared, ts.Name.Name)
				}
			}
		}
	}
	listed := make([]string, 0, len(capabilityInterfaces))
	for name := range capabilityInterfaces {
		listed = append(listed, name)
	}
	sort.Strings(declared)
	sort.Strings(listed)
	assert.Equal(t, declared, listed)
}

// Every capability the served SQLiteGraph has, the lazyGraph that wraps it in
// serve mode forwards.
func TestLazyGraph_ForwardsEveryCapabilityOfSQLiteGraph(t *testing.T) {
	sqlite := reflect.TypeFor[*graph.SQLiteGraph]()
	lazy := reflect.TypeFor[*lazyGraph]()
	for name, iface := range capabilityInterfaces {
		if sqlite.Implements(iface) {
			assert.True(t, lazy.Implements(iface), "*lazyGraph does not forward %s, which *graph.SQLiteGraph implements", name)
		}
	}
}
