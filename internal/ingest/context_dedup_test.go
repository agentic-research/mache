package ingest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #3: context file contains duplicate declarations
// When multiple constructs from the same file share context,
// the context should not contain duplicate entries.

func TestEngine_ContextNoDuplicates(t *testing.T) {
	schema := loadGoSchema(t)
	tmpDir := t.TempDir()

	// Create a Go file with multiple constructs sharing the same file scope.
	// The context (imports, types, vars) should appear ONCE, not once per construct.
	goContent := `package doer

import "fmt"

type SessionMapper interface {
	Map(id string) string
}

var sessionID = "default"

func DoWork() {
	fmt.Println(sessionID)
}

func DoMore() {
	fmt.Println(sessionID)
}

func DoExtra() {
	fmt.Println(sessionID)
}
`
	goFile := filepath.Join(tmpDir, "doer.go")
	err := os.WriteFile(goFile, []byte(goContent), 0o644)
	require.NoError(t, err)

	store := graph.NewMemoryStore()
	engine := NewEngine(schema, store)
	require.NoError(t, engine.Ingest(tmpDir))

	// Check context of any construct directory — should not have duplicates
	doWorkDir, err := store.GetNode("doer/functions/DoWork")
	require.NoError(t, err)
	require.NotNil(t, doWorkDir.Context, "DoWork should have context")

	ctx := string(doWorkDir.Context)

	// Count occurrences of SessionMapper — should appear exactly once
	count := strings.Count(ctx, "SessionMapper")
	assert.Equal(t, 1, count, "SessionMapper should appear once in context, got %d", count)

	// Count occurrences of sessionID declaration — should appear exactly once
	count = strings.Count(ctx, `var sessionID`)
	assert.Equal(t, 1, count, "sessionID declaration should appear once in context, got %d", count)

	// import "fmt" should appear once
	count = strings.Count(ctx, `import "fmt"`)
	assert.Equal(t, 1, count, "import should appear once in context")
}

// Issue #3 variant: context across multiple files in same package
func TestEngine_ContextMultiFileMerge(t *testing.T) {
	schema := loadGoSchema(t)
	tmpDir := t.TempDir()

	// Two Go files in the same package — context should merge without duplicates
	file1 := `package pkg

import "fmt"

type Shared struct{}

func FuncA() {
	fmt.Println("a")
}
`
	file2 := `package pkg

import "fmt"

type Shared2 struct{}

func FuncB() {
	fmt.Println("b")
}
`
	err := os.WriteFile(filepath.Join(tmpDir, "a.go"), []byte(file1), 0o644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(tmpDir, "b.go"), []byte(file2), 0o644)
	require.NoError(t, err)

	store := graph.NewMemoryStore()
	engine := NewEngine(schema, store)
	require.NoError(t, engine.Ingest(tmpDir))

	// FuncA's context comes from a.go — should have Shared but not Shared2
	funcADir, err := store.GetNode("pkg/functions/FuncA")
	require.NoError(t, err)
	require.NotNil(t, funcADir.Context)

	ctxA := string(funcADir.Context)
	assert.Contains(t, ctxA, "Shared")

	// FuncB's context comes from b.go — should have Shared2 but not Shared
	funcBDir, err := store.GetNode("pkg/functions/FuncB")
	require.NoError(t, err)
	require.NotNil(t, funcBDir.Context)

	ctxB := string(funcBDir.Context)
	assert.Contains(t, ctxB, "Shared2")
}
