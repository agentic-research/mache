// Tests for `mache cache inspect` + --token-file resolution.

package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── inspect ──────────────────────────────────────────────────────

func TestCacheInspect_LocalCacheDir(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "input.db")
	outDir := filepath.Join(tmp, "out")
	makeSyntheticDB(t, dbPath, []synthSource{
		{id: "a.go", path: "a.go", language: "go", content: []byte("package a\n")},
		{id: "b.go", path: "b.go", language: "go", content: []byte("package b\n")},
	})
	if err := runCachePush(new(bytes.Buffer), dbPath, outDir); err != nil {
		t.Fatalf("push: %v", err)
	}

	var buf bytes.Buffer
	if err := runCacheInspect(&buf, outDir); err != nil {
		t.Fatalf("inspect: %v\n%s", err, buf.String())
	}
	out := buf.String()
	for _, want := range []string{
		"producer:",
		"mache",
		"schema_version:",
		CacheVersion,
		"sources:",
		"chunks on disk:",
		"raw-shape (Phase 1):",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("inspect output missing %q:\n%s", want, out)
		}
	}
}

func TestCacheInspect_BareLockfile(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "input.db")
	outDir := filepath.Join(tmp, "out")
	makeSyntheticDB(t, dbPath, []synthSource{
		{id: "a.go", path: "a.go", language: "go", content: []byte("package a\n")},
	})
	if err := runCachePush(new(bytes.Buffer), dbPath, outDir); err != nil {
		t.Fatalf("push: %v", err)
	}

	// Inspect the bare lockfile (not the dir). Chunks-on-disk
	// section should NOT appear (no chunks-dir context).
	binPath := filepath.Join(outDir, "mache.lock.bin")
	var buf bytes.Buffer
	if err := runCacheInspect(&buf, binPath); err != nil {
		t.Fatalf("inspect bin: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "producer:") {
		t.Errorf("bin-only inspect should print meta:\n%s", out)
	}
	if strings.Contains(out, "chunks on disk:") {
		t.Errorf("bin-only inspect should NOT enumerate chunks:\n%s", out)
	}
}

func TestCacheInspect_MissingChunks(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "input.db")
	outDir := filepath.Join(tmp, "out")
	makeSyntheticDB(t, dbPath, []synthSource{
		{id: "a.go", path: "a.go", language: "go", content: []byte("package a\n")},
	})
	if err := runCachePush(new(bytes.Buffer), dbPath, outDir); err != nil {
		t.Fatalf("push: %v", err)
	}

	// Delete the chunks dir to simulate corruption.
	if err := os.RemoveAll(filepath.Join(outDir, "objects")); err != nil {
		t.Fatalf("rm objects: %v", err)
	}
	// Recreate empty so the path exists but is empty.
	_ = os.MkdirAll(filepath.Join(outDir, "objects"), 0o755)

	var buf bytes.Buffer
	err := runCacheInspect(&buf, outDir)
	if err == nil {
		t.Fatalf("inspect should fail when chunks missing")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("expected 'missing' in error; got %v", err)
	}
}

func TestCacheInspect_ASTBundle(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "input.db")
	outDir := filepath.Join(tmp, "out")
	makeSyntheticDBWithAST(t, dbPath,
		[]synthSource{
			{id: "a.go", path: "a.go", language: "go", content: []byte("package a\n")},
		},
		[]synthAstNode{
			{nodeID: "a.go/x", sourceID: "a.go", nodeKind: "x", startByte: 0, endByte: 1},
		},
	)
	if err := runCachePush(new(bytes.Buffer), dbPath, outDir); err != nil {
		t.Fatalf("push: %v", err)
	}
	var buf bytes.Buffer
	if err := runCacheInspect(&buf, outDir); err != nil {
		t.Fatalf("inspect ast: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "ast-shape (Phase 4): 1") {
		t.Errorf("AST bundle should report 1 ast-shape chunk:\n%s", out)
	}
}

// ── --token-file resolution ──────────────────────────────────────

func TestResolveCacheToken_FileTakesPriority(t *testing.T) {
	tmp := t.TempDir()
	tokenPath := filepath.Join(tmp, "token")
	if err := os.WriteFile(tokenPath, []byte("from-file\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	got, err := resolveCacheToken("from-cli", tokenPath)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "from-file" {
		t.Errorf("--token-file should take priority over --token; got %q", got)
	}
}

func TestResolveCacheToken_FileTrimming(t *testing.T) {
	tmp := t.TempDir()
	tokenPath := filepath.Join(tmp, "token")
	// Trailing newline + tab whitespace should all get trimmed.
	if err := os.WriteFile(tokenPath, []byte("\tsecret-token-value  \n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	got, err := resolveCacheToken("", tokenPath)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "secret-token-value" {
		t.Errorf("--token-file value not trimmed: got %q", got)
	}
}

func TestResolveCacheToken_EmptyFileErrors(t *testing.T) {
	tmp := t.TempDir()
	tokenPath := filepath.Join(tmp, "token")
	if err := os.WriteFile(tokenPath, []byte("\n  \t\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	_, err := resolveCacheToken("", tokenPath)
	if err == nil {
		t.Fatalf("empty token file should error; got nil")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("expected 'empty' in error; got %v", err)
	}
}

func TestResolveCacheToken_FallbackToCLI(t *testing.T) {
	got, err := resolveCacheToken("cli-only", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "cli-only" {
		t.Errorf("want cli-only, got %q", got)
	}
}

func TestResolveCacheToken_FallbackToEnv(t *testing.T) {
	t.Setenv("MACHE_CACHE_TOKEN", "env-value")
	got, err := resolveCacheToken("", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "env-value" {
		t.Errorf("want env-value, got %q", got)
	}
}

func TestResolveCacheToken_AllEmpty(t *testing.T) {
	t.Setenv("MACHE_CACHE_TOKEN", "")
	got, err := resolveCacheToken("", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "" {
		t.Errorf("all empty should yield empty token; got %q", got)
	}
}

func TestResolveCacheToken_MissingFile(t *testing.T) {
	_, err := resolveCacheToken("", "/nonexistent/path/to/token")
	if err == nil {
		t.Fatalf("missing token file should error; got nil")
	}
}
