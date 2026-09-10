package ingest

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/agentic-research/mache/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cachedSources counts entries across every per-file cache the walker keeps.
func (w *ASTWalker) cachedSources() int {
	n := 0
	for _, m := range []*sync.Map{
		&w.indexCache, &w.sourceCache, &w.langCache, &w.pkgCache, &w.addrRefCache, &w.callTokenCache,
	} {
		m.Range(func(_, _ any) bool { n++; return true })
	}
	return n
}

// TestEngine_Ingest_EvictsWalkerCachesPerFile is the memory-class gate for
// mache-95a33d: a one-shot build must not retain every file's ASTWalker cache
// until the walker dies. On a 929-file repo that retention was 650 MB of a
// 1.7 GB peak — the difference between a laptop and a workstation.
//
// Asserted as a COUNT of retained cache entries rather than an RSS figure, for
// the same reason the growth gate counts statements instead of milliseconds:
// the failure signature is "entries survive the file's projection", and zero
// is a sharper thing to assert than a number of bytes on a particular machine.
func TestEngine_Ingest_EvictsWalkerCachesPerFile(t *testing.T) {
	schema := loadGoSchema(t)
	tmpDir := t.TempDir()
	for name, src := range map[string]string{
		"a.go": "package shared\n\nfunc FuncA() { FuncB() }\n",
		"b.go": "package shared\n\nfunc FuncB() {}\n",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(tmpDir, name), []byte(src), 0o644))
	}

	store := graph.NewMemoryStore()
	engine := NewEngine(schema, store)
	attachLeylineAST(t, engine, tmpDir)
	require.NoError(t, engine.Ingest(tmpDir))

	// Non-vacuity: the walker did project both files through its caches.
	_, err := store.GetNode("shared/functions/FuncA/source")
	require.NoError(t, err)
	_, err = store.GetNode("shared/functions/FuncB/source")
	require.NoError(t, err)

	assert.Zero(t, engine.astWalker.cachedSources(),
		"ASTWalker retained per-file cache entries after the build finished")
}
