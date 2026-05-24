// End-to-end test for Phase 3: local push → remote push → fresh
// local pull from remote → restore. Uses the in-process mock
// registry from cache_oci_test.go.
//
// Why a separate file: cache_oci_test.go tests the OCI client in
// isolation; this file tests the full glue that runCacheRemotePush
// / runCacheRemotePull provide on top of it.

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestCacheRemoteRoundTrip(t *testing.T) {
	srv, _, _ := startMock(t)
	ctx := context.Background()

	// 1. Set up a synthetic db.
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "input.db")
	pushDir := filepath.Join(tmp, "push-out")
	pullDir := filepath.Join(tmp, "pull-in")
	restoredPath := filepath.Join(tmp, "restored.db")

	original := []synthSource{
		{id: "a.go", path: "a.go", language: "go", content: []byte("package a\n")},
		{id: "b.go", path: "b.go", language: "go", content: []byte("package b\n")},
	}
	makeSyntheticDB(t, dbPath, original)

	// 2. Local push (Phase 1 still has to run before remote push).
	var buf bytes.Buffer
	if err := runCachePush(&buf, dbPath, pushDir); err != nil {
		t.Fatalf("local push: %v", err)
	}

	// 3. Remote push.
	if err := runCacheRemotePush(ctx, &buf, pushDir, srv.URL, "mache", "e2e-scope", "latest", ""); err != nil {
		t.Fatalf("remote push: %v\n%s", err, buf.String())
	}

	// 4. Remote pull into a fresh local dir.
	if err := runCacheRemotePull(ctx, &buf, srv.URL, "mache", "e2e-scope", "latest", "", pullDir); err != nil {
		t.Fatalf("remote pull: %v\n%s", err, buf.String())
	}

	// 5. Local pull restores from pullDir.
	if err := runCachePull(&buf, pullDir, restoredPath, true); err != nil {
		t.Fatalf("local pull: %v\n%s", err, buf.String())
	}

	// 6. Verify restored content matches.
	restored := readBackSources(t, restoredPath)
	if len(restored) != len(original) {
		t.Fatalf("restored count: want %d, got %d", len(original), len(restored))
	}
	pathToOrig := map[string][]byte{}
	for _, r := range original {
		pathToOrig[r.path] = r.content
	}
	for _, r := range restored {
		want, ok := pathToOrig[r.path]
		if !ok {
			t.Errorf("unexpected restored path %s", r.path)
			continue
		}
		if !bytes.Equal(r.content, want) {
			t.Errorf("content drift for %s: want %q, got %q", r.path, want, r.content)
		}
	}
}

func TestCacheRemotePush_RequiresScope(t *testing.T) {
	srv, _, _ := startMock(t)
	ctx := context.Background()
	tmp := t.TempDir()
	pushDir := filepath.Join(tmp, "out")
	// Empty pushDir — function would fail on missing lockfile, but
	// the scope-required check is up to the CLI layer not the func.
	// This test exercises that an empty baseURL is rejected by the
	// constructor — the more interesting validation.
	err := runCacheRemotePush(ctx, new(bytes.Buffer), pushDir, "", "mache", "", "", "")
	if err == nil {
		t.Fatalf("empty baseURL should fail; got nil")
	}
	_ = srv // keep the linter happy
}

func TestCacheRemotePush_IdempotentSecondRun(t *testing.T) {
	srv, reg, _ := startMock(t)
	ctx := context.Background()

	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "input.db")
	pushDir := filepath.Join(tmp, "push-out")
	makeSyntheticDB(t, dbPath, []synthSource{
		{id: "x.go", path: "x.go", language: "go", content: []byte("package x\n")},
	})
	if err := runCachePush(new(bytes.Buffer), dbPath, pushDir); err != nil {
		t.Fatalf("local push: %v", err)
	}

	if err := runCacheRemotePush(ctx, new(bytes.Buffer), pushDir, srv.URL, "mache", "idem-scope", "latest", ""); err != nil {
		t.Fatalf("first remote push: %v", err)
	}

	reg.mu.Lock()
	blobCount := len(reg.blobs)
	manifestCount := len(reg.manifests)
	reg.mu.Unlock()

	// Second remote push: HEAD on every blob says "already present",
	// no re-upload. Manifest count may grow by 1 (digest already
	// present is overwritten with same body; tag stays at "latest").
	if err := runCacheRemotePush(ctx, new(bytes.Buffer), pushDir, srv.URL, "mache", "idem-scope", "latest", ""); err != nil {
		t.Fatalf("second remote push: %v", err)
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if len(reg.blobs) != blobCount {
		t.Errorf("blob count drift on idempotent push: %d → %d", blobCount, len(reg.blobs))
	}
	// Manifest count should also be stable (digest + tag both unchanged).
	if len(reg.manifests) != manifestCount {
		t.Errorf("manifest count drift on idempotent push: %d → %d", manifestCount, len(reg.manifests))
	}
}

// ── verify subcommand (Phase 3.5 — CI-friendly existence check) ────

func TestCacheVerify_IntactBundle(t *testing.T) {
	srv, _, _ := startMock(t)
	ctx := context.Background()

	// Push a bundle first.
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "input.db")
	pushDir := filepath.Join(tmp, "out")
	makeSyntheticDB(t, dbPath, []synthSource{
		{id: "x.go", path: "x.go", language: "go", content: []byte("package x\n")},
		{id: "y.go", path: "y.go", language: "go", content: []byte("package y\n")},
	})
	if err := runCachePush(new(bytes.Buffer), dbPath, pushDir); err != nil {
		t.Fatalf("local push: %v", err)
	}
	if err := runCacheRemotePush(ctx, new(bytes.Buffer), pushDir, srv.URL, "mache", "verify-scope", "latest", ""); err != nil {
		t.Fatalf("remote push: %v", err)
	}

	// Verify should pass cleanly.
	var buf bytes.Buffer
	if err := runCacheVerify(ctx, &buf, srv.URL, "mache", "verify-scope", "latest", ""); err != nil {
		t.Fatalf("verify: %v\n%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "bundle") || !strings.Contains(out, "intact") {
		t.Errorf("verify output missing success markers: %s", out)
	}
}

func TestCacheVerify_MissingManifest(t *testing.T) {
	srv, _, _ := startMock(t)
	ctx := context.Background()

	err := runCacheVerify(ctx, new(bytes.Buffer), srv.URL, "mache", "no-such-scope", "latest", "")
	if err == nil {
		t.Fatalf("verify should fail when manifest missing; got nil")
	}
	if !strings.Contains(err.Error(), "verify manifest") {
		t.Errorf("expected 'verify manifest' in error; got %v", err)
	}
}

func TestCacheVerify_MissingLayer(t *testing.T) {
	srv, reg, _ := startMock(t)
	ctx := context.Background()

	// Push a real bundle, then delete one layer from the mock.
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "input.db")
	pushDir := filepath.Join(tmp, "out")
	makeSyntheticDB(t, dbPath, []synthSource{
		{id: "a.go", path: "a.go", language: "go", content: []byte("package a\n")},
		{id: "b.go", path: "b.go", language: "go", content: []byte("package b\n")},
	})
	if err := runCachePush(new(bytes.Buffer), dbPath, pushDir); err != nil {
		t.Fatalf("local push: %v", err)
	}
	if err := runCacheRemotePush(ctx, new(bytes.Buffer), pushDir, srv.URL, "mache", "del-scope", "latest", ""); err != nil {
		t.Fatalf("remote push: %v", err)
	}

	// Delete one layer blob from the registry (simulates GC eating a chunk).
	reg.mu.Lock()
	var deletedAny bool
	for digest := range reg.blobs {
		// Skip the config (the latest pushed manifest's config). Heuristic:
		// keep the largest blob (most likely the lockfile); delete a smaller one.
		// Simpler: just delete the first blob we find that's not in the manifest's
		// config slot. We don't know which one that is here — instead, look at the
		// "latest" manifest and pick layers[0].
		_ = digest
	}
	if mfBytes, ok := reg.manifests["latest"]; ok {
		var m OCIManifest
		if err := json.Unmarshal(mfBytes, &m); err == nil && len(m.Layers) > 0 {
			delete(reg.blobs, m.Layers[0].Digest)
			deletedAny = true
		}
	}
	reg.mu.Unlock()
	if !deletedAny {
		t.Fatalf("test invariant: could not delete a layer to simulate GC")
	}

	// Verify should report the missing layer.
	var buf bytes.Buffer
	err := runCacheVerify(ctx, &buf, srv.URL, "mache", "del-scope", "latest", "")
	if err == nil {
		t.Fatalf("verify should fail when a layer is missing; got nil\n%s", buf.String())
	}
	if !strings.Contains(err.Error(), "layers missing") {
		t.Errorf("expected 'layers missing' in error; got %v", err)
	}
	if !strings.Contains(buf.String(), "MISSING layer:") {
		t.Errorf("expected 'MISSING layer:' in output; got %s", buf.String())
	}
}

func TestCacheVerify_DetectsCorruptedSampleLayer(t *testing.T) {
	srv, reg, _ := startMock(t)
	ctx := context.Background()

	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "input.db")
	pushDir := filepath.Join(tmp, "out")
	makeSyntheticDB(t, dbPath, []synthSource{
		{id: "a.go", path: "a.go", language: "go", content: []byte("package a\n")},
	})
	if err := runCachePush(new(bytes.Buffer), dbPath, pushDir); err != nil {
		t.Fatalf("local push: %v", err)
	}
	if err := runCacheRemotePush(ctx, new(bytes.Buffer), pushDir, srv.URL, "mache", "corrupt-scope", "latest", ""); err != nil {
		t.Fatalf("remote push: %v", err)
	}

	// Flip the corruptGET flag so GET returns wrong bytes.
	reg.mu.Lock()
	reg.corruptGET = true
	reg.mu.Unlock()

	err := runCacheVerify(ctx, new(bytes.Buffer), srv.URL, "mache", "corrupt-scope", "latest", "")
	if err == nil {
		t.Fatalf("verify should fail under corruptGET; got nil")
	}
	// The verify-config GET is hit first, so the error is from that path.
	if !strings.Contains(err.Error(), "verify config") && !strings.Contains(err.Error(), "integrity") {
		t.Errorf("expected verify config / integrity error; got %v", err)
	}
}
