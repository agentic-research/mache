package ingest

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A selector's `field:` labels are constraints, not decoration (mache-91d903).
//
// `(method_declaration receiver: (parameter_list (parameter_declaration type:
// (type_identifier) @receiver)) ...)` names the RECEIVER's parameter_list. A
// method's parameters are a second parameter_list with the same kind chain, so
// a walker that matches by kind alone also resolves @receiver to a plain-typed
// parameter and projects a phantom `<paramtype>.<method>` — `string.Add` for
// `func (s *Store) Add(key string, item Item)` — alongside the real one. The
// golden corpus carried exactly that phantom until this test existed.
//
// This projects through the real go preset on a db the pinned leyline parsed:
// the field names come from ley-line's node_child table, so the whole seam is
// exercised, not a hand-built fixture.
func TestProjectSourceFile_FieldLabelsConstrainCaptures(t *testing.T) {
	const src = `package store

type Store struct{}
type Item struct{}

// Add: pointer receiver; a plain-typed parameter (string) that the value-
// receiver selector must NOT resolve as the receiver.
func (s *Store) Add(key string, item Item) {}

// Put: value receiver; a pointer-typed parameter (*Item) that the pointer-
// receiver selector must NOT resolve as the receiver.
func (v Store) Put(p *Item) {}

// Same: both parameter lists are the same subtree (Store) — identical
// content hashes — so only the field name tells receiver from parameters.
func (Store) Same(Store) {}
`
	p := newProjectionProbe(t, "../../schema/presets/go.json", map[string]string{
		"store.go": src,
	})
	p.project(t, "store.go", "go")

	// The store records a file's leaves: <pkg>/methods/<Recv.Name>/source.
	var methods []string
	for _, id := range p.store.NodesForPath(filepath.Join(p.dir, "store.go")) {
		if rest, ok := strings.CutPrefix(id, "store/methods/"); ok {
			methods = append(methods, strings.TrimSuffix(rest, "/source"))
		}
	}
	sort.Strings(methods)
	assert.Equal(t, []string{"Store.Add", "Store.Put", "Store.Same"}, methods,
		"every method must project under its RECEIVER type once — a parameter type is never a receiver")
	require.NotContains(t, methods, "string.Add")
	require.NotContains(t, methods, "Item.Put")
}
