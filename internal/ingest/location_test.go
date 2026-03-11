package ingest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #5: No file:line metadata on projected nodes
// Each projected node should have a "location" property with file:line info.

func TestEngine_LocationMetadata(t *testing.T) {
	schema := loadGoSchema(t)
	tmpDir := t.TempDir()

	// Create a Go file with multiple constructs at known line numbers
	goContent := `package main

import "fmt"

const MaxRetries = 3

type Config struct {
	Name string
}

func Hello() {
	fmt.Println("hello")
}

func Goodbye() {
	fmt.Println("goodbye")
}
`
	goFile := filepath.Join(tmpDir, "main.go")
	err := os.WriteFile(goFile, []byte(goContent), 0o644)
	require.NoError(t, err)

	store := graph.NewMemoryStore()
	engine := NewEngine(schema, store)
	require.NoError(t, engine.Ingest(tmpDir))

	// Check that Hello function has location metadata
	helloSource, err := store.GetNode("main/functions/Hello/source")
	require.NoError(t, err, "Hello function should exist")
	require.NotNil(t, helloSource.Origin, "Hello should have a SourceOrigin")

	// The location should be convertible to line numbers
	loc := ByteOffsetToLocation(goContent, helloSource.Origin)
	assert.Equal(t, "main.go", filepath.Base(loc.FilePath))
	assert.Equal(t, 11, loc.StartLine, "Hello starts at line 11")
	assert.Equal(t, 13, loc.EndLine, "Hello ends at line 13")

	// Check Goodbye
	goodbyeSource, err := store.GetNode("main/functions/Goodbye/source")
	require.NoError(t, err)
	require.NotNil(t, goodbyeSource.Origin)
	loc = ByteOffsetToLocation(goContent, goodbyeSource.Origin)
	assert.Equal(t, 15, loc.StartLine, "Goodbye starts at line 15")
	assert.Equal(t, 17, loc.EndLine, "Goodbye ends at line 17")

	// Check that const also has location
	constSource, err := store.GetNode("main/constants/MaxRetries/source")
	require.NoError(t, err)
	require.NotNil(t, constSource.Origin)
	loc = ByteOffsetToLocation(goContent, constSource.Origin)
	assert.Equal(t, 5, loc.StartLine, "MaxRetries at line 5")

	// Check that Config type has location
	configSource, err := store.GetNode("main/types/Config/source")
	require.NoError(t, err)
	require.NotNil(t, configSource.Origin)
	loc = ByteOffsetToLocation(goContent, configSource.Origin)
	assert.Equal(t, 7, loc.StartLine, "Config starts at line 7")
}

// Location represents a human-readable source location.
type Location struct {
	FilePath  string
	StartLine int
	EndLine   int
}

// ByteOffsetToLocation converts a SourceOrigin's byte offsets to line numbers.
// content is the full file content. Returns 1-based line numbers.
func ByteOffsetToLocation(content string, origin *graph.SourceOrigin) Location {
	startLine := 1
	endLine := 1
	for i, ch := range content {
		if uint32(i) == origin.StartByte {
			break
		}
		if ch == '\n' {
			startLine++
		}
	}
	for i, ch := range content {
		if uint32(i) >= origin.EndByte {
			break
		}
		if ch == '\n' {
			endLine++
		}
	}
	return Location{
		FilePath:  origin.FilePath,
		StartLine: startLine,
		EndLine:   endLine,
	}
}

// TestLocationVirtualFile tests that a "location" virtual file is readable
// for projected source code nodes.
func TestLocationVirtualFile(t *testing.T) {
	schema := loadGoSchema(t)
	tmpDir := t.TempDir()

	goContent := `package main

func Hello() {
	return
}
`
	goFile := filepath.Join(tmpDir, "main.go")
	err := os.WriteFile(goFile, []byte(goContent), 0o644)
	require.NoError(t, err)

	store := graph.NewMemoryStore()
	engine := NewEngine(schema, store)
	require.NoError(t, engine.Ingest(tmpDir))

	// The construct directory should have a "location" property
	helloDir, err := store.GetNode("main/functions/Hello")
	require.NoError(t, err)

	// Check for location in Properties
	locData, ok := helloDir.Properties["location"]
	require.True(t, ok, "Hello directory should have 'location' property")

	// Location should be in format "relative/path.go:startline:endline"
	expected := "main.go:3:5"
	assert.Equal(t, expected, string(locData))
}
