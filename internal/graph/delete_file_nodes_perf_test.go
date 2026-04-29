package graph

import (
	"fmt"
	"io/fs"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestParentOfNodeID covers the small helper that derives a parent ID
// by stripping the last `/segment`. Required for the targeted child
// pruning in deleteFileNodes (bead mache-07f9ca).
func TestParentOfNodeID(t *testing.T) {
	cases := map[string]string{
		"":                     "",
		"top":                  "",
		"a/b":                  "a",
		"a/b/c":                "a/b",
		"a/b/c/d":              "a/b/c",
		"pkg/auth/source":      "pkg/auth",
		"single/":              "single", // trailing slash → parent is "single"
		"/leading":             "",
		"/":                    "",
		"x/y/z/very/deep/leaf": "x/y/z/very/deep",
	}
	for in, want := range cases {
		assert.Equalf(t, want, parentOfNodeID(in), "parentOfNodeID(%q)", in)
	}
}

// buildBenchStore creates a synthetic store with `nFiles` source files,
// each contributing `perFile` nodes under a parent dir. Used by the
// DeleteFileNodes benchmark to measure the targeted-pruning win.
func buildBenchStore(b *testing.B, nFiles, perFile int) (*MemoryStore, []string) {
	b.Helper()
	store := NewMemoryStore()

	parentID := "pkg"
	parent := &Node{ID: parentID, Mode: fs.ModeDir}
	parent.Children = make([]string, 0, nFiles*perFile)
	store.AddRoot(parent)

	files := make([]string, 0, nFiles)
	for f := 0; f < nFiles; f++ {
		filePath := fmt.Sprintf("/src/file_%04d.go", f)
		files = append(files, filePath)
		for i := 0; i < perFile; i++ {
			id := fmt.Sprintf("%s/file%04d_node%03d", parentID, f, i)
			parent.Children = append(parent.Children, id)
			store.AddNode(&Node{
				ID:   id,
				Mode: 0,
				Origin: &SourceOrigin{
					FilePath:  filePath,
					StartByte: uint32(i * 10),
					EndByte:   uint32(i*10 + 9),
				},
			})
		}
	}
	return store, files
}

// BenchmarkDeleteFileNodes_OneFile measures the cost of deleting one
// file's worth of nodes from a store with `nFiles` total files. Bead
// mache-07f9ca: prior implementation iterated all remaining nodes to
// prune Children references; targeted version only touches the deleted
// nodes' parent.
//
// Each b.N iteration deletes one file. The store is rebuilt once per
// sub-bench (outside the timer) and we re-add the deleted file before
// the next iteration so the working set stays at `nFiles`.
func BenchmarkDeleteFileNodes_OneFile(b *testing.B) {
	const perFile = 20
	for _, nFiles := range []int{100, 1_000, 10_000} {
		b.Run(fmt.Sprintf("files=%d", nFiles), func(b *testing.B) {
			store, files := buildBenchStore(b, nFiles, perFile)
			target := files[len(files)/2]

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				store.DeleteFileNodes(target)
				b.StopTimer()
				readdFile(store, target, perFile)
				b.StartTimer()
			}
		})
	}
}

// readdFile re-creates the perFile nodes for filePath after a delete,
// so the next benchmark iteration has the same working-set size as the
// first. The added nodes are wired back into the same parent (`pkg`).
func readdFile(store *MemoryStore, filePath string, perFile int) {
	parent, err := store.GetNode("pkg")
	if err != nil {
		return
	}
	parentClone := *parent
	parentClone.Children = append([]string(nil), parent.Children...)

	idPrefix := fmt.Sprintf("pkg/%s_node", lastSegment(filePath))
	for i := 0; i < perFile; i++ {
		id := fmt.Sprintf("%s%03d", idPrefix, i)
		parentClone.Children = append(parentClone.Children, id)
		store.AddNode(&Node{
			ID:   id,
			Mode: 0,
			Origin: &SourceOrigin{
				FilePath:  filePath,
				StartByte: uint32(i * 10),
				EndByte:   uint32(i*10 + 9),
			},
		})
	}
	store.AddNode(&parentClone)
}

func lastSegment(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}
