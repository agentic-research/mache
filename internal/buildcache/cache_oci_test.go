// OCI client tests for build-cache/v1 transport (Phase 3).
//
// Uses httptest to mock the registry surface. Tests run hermetically
// against an in-process HTTP server; no Docker / real registry needed.
//
// What the mock implements:
//   - HEAD /v2/.../blobs/<digest>          → 200 (present) / 404 (absent)
//   - POST /v2/.../blobs/uploads/          → 202 Location: <upload>
//   - PUT  /v2/.../blobs/uploads/<id>      → 201, with digest validation
//   - GET  /v2/.../blobs/<digest>          → 200 or 404
//   - PUT  /v2/.../manifests/<ref>         → 201
//   - GET  /v2/.../manifests/<ref>         → 200 or 404
//
// Concurrency-safe: mutexed maps for blobs + manifests so the
// parallel-upload tests don't race.

package buildcache

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/zeebo/blake3"
)

// ── mock registry ─────────────────────────────────────────────────

type mockRegistry struct {
	mu        sync.Mutex
	blobs     map[string][]byte // digest → body
	manifests map[string][]byte // ref → body
	uploadIDs map[string]string // upload-uuid → ""(reserved)
	uploadSeq int
	// Failure injection.
	failHEAD   bool // every HEAD returns 500
	failPUT    bool // every PUT returns 500
	corruptGET bool // GET returns wrong bytes (drift detection)
}

func newMockRegistry() *mockRegistry {
	return &mockRegistry{
		blobs:     make(map[string][]byte),
		manifests: make(map[string][]byte),
		uploadIDs: make(map[string]string),
	}
}

func (m *mockRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Routing: /v2/<producer>/<scope>/(blobs|manifests|blobs/uploads)/...
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/blobs/uploads/") && r.Method == http.MethodPost:
		m.handleUploadInit(w, r)
	case strings.Contains(path, "/blobs/uploads/") && r.Method == http.MethodPut:
		m.handleUploadPUT(w, r)
	case strings.Contains(path, "/blobs/") && r.Method == http.MethodHead:
		m.handleBlobHEAD(w, r)
	case strings.Contains(path, "/blobs/") && r.Method == http.MethodGet:
		m.handleBlobGET(w, r)
	case strings.Contains(path, "/manifests/") && r.Method == http.MethodPut:
		m.handleManifestPUT(w, r)
	case strings.Contains(path, "/manifests/") && r.Method == http.MethodGet:
		m.handleManifestGET(w, r)
	default:
		http.Error(w, "mock: unsupported route "+r.Method+" "+path, http.StatusBadRequest)
	}
}

// digestFromPath extracts the trailing path segment as a digest.
func digestFromPath(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

func (m *mockRegistry) handleBlobHEAD(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failHEAD {
		http.Error(w, "mock failure", http.StatusInternalServerError)
		return
	}
	digest := digestFromPath(r.URL.Path)
	if _, ok := m.blobs[digest]; ok {
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Error(w, "blob unknown", http.StatusNotFound)
}

func (m *mockRegistry) handleBlobGET(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	digest := digestFromPath(r.URL.Path)
	body, ok := m.blobs[digest]
	if !ok {
		http.Error(w, "blob unknown", http.StatusNotFound)
		return
	}
	if m.corruptGET {
		body = []byte("CORRUPTED ON THE WIRE")
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (m *mockRegistry) handleUploadInit(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.uploadSeq++
	uuid := fmt.Sprintf("u-%d", m.uploadSeq)
	m.uploadIDs[uuid] = ""
	// Construct the Location URL by keeping the request path prefix
	// (which already includes /v2/<producer>/<scope>/blobs/uploads/)
	// and appending the UUID. The PUT routing in ServeHTTP keys off
	// the substring "/blobs/uploads/", so the path must contain it.
	location := r.URL.Path + uuid
	w.Header().Set("Location", location)
	w.WriteHeader(http.StatusAccepted)
}

func (m *mockRegistry) handleUploadPUT(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failPUT {
		http.Error(w, "mock failure", http.StatusInternalServerError)
		return
	}
	digest := r.URL.Query().Get("digest")
	if digest == "" {
		http.Error(w, "missing ?digest=", http.StatusBadRequest)
		return
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r.Body); err != nil {
		http.Error(w, fmt.Sprintf("read upload body: %v", err), http.StatusBadRequest)
		return
	}
	full := buf.Bytes()

	// Validate: BLAKE3(body) must match digest (per the v1 wire encoding).
	expected, ok := strings.CutPrefix(digest, "sha256:")
	if !ok {
		http.Error(w, "digest missing sha256: prefix", http.StatusBadRequest)
		return
	}
	actual := blake3.Sum256(full)
	if hex.EncodeToString(actual[:]) != expected {
		http.Error(w, "BLOB_DIGEST_MISMATCH", http.StatusBadRequest)
		return
	}

	m.blobs[digest] = full
	w.Header().Set("Docker-Content-Digest", digest)
	w.WriteHeader(http.StatusCreated)
}

func (m *mockRegistry) handleManifestPUT(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failPUT {
		http.Error(w, "mock failure", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r.Body)
	ref := digestFromPath(r.URL.Path)
	m.manifests[ref] = buf.Bytes()
	w.WriteHeader(http.StatusCreated)
}

func (m *mockRegistry) handleManifestGET(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ref := digestFromPath(r.URL.Path)
	body, ok := m.manifests[ref]
	if !ok {
		http.Error(w, "manifest unknown", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", ociManifestMediaType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// ── client tests ──────────────────────────────────────────────────

func startMock(t *testing.T) (*httptest.Server, *mockRegistry, *OCIClient) {
	t.Helper()
	reg := newMockRegistry()
	srv := httptest.NewServer(reg)
	t.Cleanup(srv.Close)
	client, err := NewOCIClient(srv.URL, "mache", "test-scope")
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return srv, reg, client
}

func TestOCI_PutGetBlob_RoundTrip(t *testing.T) {
	_, _, c := startMock(t)
	ctx := context.Background()
	body := []byte("a simple blob")
	digest := digestFor(blake3.Sum256(body))

	if err := c.PutBlob(ctx, digest, body); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}

	got, err := c.GetBlob(ctx, digest)
	if err != nil {
		t.Fatalf("GetBlob: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("blob round-trip drift: want %q, got %q", body, got)
	}
}

func TestOCI_HeadBlob_PresentAndAbsent(t *testing.T) {
	_, _, c := startMock(t)
	ctx := context.Background()
	body := []byte("present")
	digest := digestFor(blake3.Sum256(body))

	// Absent first.
	ok, err := c.HeadBlob(ctx, digest)
	if err != nil {
		t.Fatalf("HEAD absent: %v", err)
	}
	if ok {
		t.Errorf("HEAD should report absent before put")
	}

	_ = c.PutBlob(ctx, digest, body)

	// Present after put.
	ok, err = c.HeadBlob(ctx, digest)
	if err != nil {
		t.Fatalf("HEAD present: %v", err)
	}
	if !ok {
		t.Errorf("HEAD should report present after put")
	}
}

func TestOCI_PutBlob_Idempotent(t *testing.T) {
	_, reg, c := startMock(t)
	ctx := context.Background()
	body := []byte("idempotency target")
	digest := digestFor(blake3.Sum256(body))

	_ = c.PutBlob(ctx, digest, body)
	// Second put: HEAD short-circuits before upload.
	if err := c.PutBlob(ctx, digest, body); err != nil {
		t.Fatalf("second PutBlob: %v", err)
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if len(reg.blobs) != 1 {
		t.Errorf("blob count after idempotent put: want 1, got %d", len(reg.blobs))
	}
}

func TestOCI_GetBlob_DetectsCorruption(t *testing.T) {
	_, reg, c := startMock(t)
	ctx := context.Background()
	body := []byte("original")
	digest := digestFor(blake3.Sum256(body))
	_ = c.PutBlob(ctx, digest, body)

	// Flip corruption flag.
	reg.mu.Lock()
	reg.corruptGET = true
	reg.mu.Unlock()

	_, err := c.GetBlob(ctx, digest)
	if err == nil {
		t.Fatalf("GetBlob should reject corrupted body; got nil error")
	}
	if !strings.Contains(err.Error(), "integrity violation") {
		t.Errorf("expected integrity violation; got %v", err)
	}
}

func TestOCI_GetBlob_404(t *testing.T) {
	_, _, c := startMock(t)
	ctx := context.Background()
	digest := digestFor(blake3.Sum256([]byte("never put")))
	_, err := c.GetBlob(ctx, digest)
	var missing *OCIBlobMissingError
	if !errors.As(err, &missing) {
		t.Fatalf("want OCIBlobMissingError, got %T: %v", err, err)
	}
	if missing.Digest != digest {
		t.Errorf("missing.Digest: want %s, got %s", digest, missing.Digest)
	}
}

func TestOCI_PullBundle_RejectsWrongMediaType(t *testing.T) {
	_, reg, c := startMock(t)
	ctx := context.Background()

	// Inject a manifest with WRONG config.mediaType.
	bad := OCIManifest{
		SchemaVersion: 2,
		MediaType:     ociManifestMediaType,
		Config: OCIDescriptor{
			MediaType: "application/vnd.evil.config",
			Digest:    "sha256:" + strings.Repeat("0", 64),
			Size:      0,
		},
	}
	body, _ := json.Marshal(&bad)
	manifestDigest := digestFor(blake3.Sum256(body))
	reg.mu.Lock()
	reg.manifests[manifestDigest] = body
	reg.mu.Unlock()

	_, _, _, _, err := c.PullBundle(ctx, manifestDigest, 4)
	if err == nil {
		t.Fatalf("PullBundle should refuse wrong config.mediaType; got nil error")
	}
}

func TestOCI_PullBundle_404Manifest(t *testing.T) {
	_, _, c := startMock(t)
	ctx := context.Background()
	_, _, _, _, err := c.PullBundle(ctx, "sha256:deadbeef", 4)
	var missing *OCIManifestMissingError
	if !errors.As(err, &missing) {
		t.Fatalf("want OCIManifestMissingError, got %T: %v", err, err)
	}
}

func TestOCI_PushPullBundle_RoundTrip(t *testing.T) {
	_, _, c := startMock(t)
	ctx := context.Background()

	// Build a 3-chunk bundle.
	chunks := map[string][]byte{}
	var layers []OCIDescriptor
	for i := 0; i < 3; i++ {
		body := []byte(fmt.Sprintf("chunk content %d", i))
		digest := digestFor(blake3.Sum256(body))
		chunks[digest] = body
		layers = append(layers, OCIDescriptor{
			MediaType: cacheLayerMediaType,
			Digest:    digest,
			Size:      int64(len(body)),
		})
	}

	configBytes := []byte("a stand-in for the canonical CacheLockfile bytes")
	configDigest := digestFor(blake3.Sum256(configBytes))
	manifest := &OCIManifest{
		SchemaVersion: 2,
		MediaType:     ociManifestMediaType,
		Config: OCIDescriptor{
			MediaType: cacheConfigMediaType,
			Digest:    configDigest,
			Size:      int64(len(configBytes)),
		},
		Layers: layers,
		Annotations: map[string]string{
			"org.cloister.build-cache.producer": "mache",
		},
	}

	manifestDigest, err := c.PushBundle(ctx, manifest, configBytes, chunks, "main", 2)
	if err != nil {
		t.Fatalf("PushBundle: %v", err)
	}

	// Pull by digest.
	m, gotConfig, gotChunks, gotManifestDigest, err := c.PullBundle(ctx, manifestDigest, 2)
	if err != nil {
		t.Fatalf("PullBundle: %v", err)
	}
	if gotManifestDigest != manifestDigest {
		t.Errorf("pulled manifest digest %s != pushed %s", gotManifestDigest, manifestDigest)
	}
	if !bytes.Equal(gotConfig, configBytes) {
		t.Errorf("config blob drift")
	}
	if len(gotChunks) != len(chunks) {
		t.Errorf("chunk count drift: want %d, got %d", len(chunks), len(gotChunks))
	}
	for digest, want := range chunks {
		got, ok := gotChunks[digest]
		if !ok {
			t.Errorf("missing chunk %s after pull", digest)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("chunk %s body drift", digest)
		}
	}
	if len(m.Layers) != 3 {
		t.Errorf("manifest layers: want 3, got %d", len(m.Layers))
	}

	// Pull by tag.
	_, _, _, digestViaTag, err := c.PullBundle(ctx, "main", 2)
	if err != nil {
		t.Fatalf("PullBundle by tag: %v", err)
	}
	if digestViaTag != manifestDigest {
		t.Errorf("tag pull manifest digest %s != %s", digestViaTag, manifestDigest)
	}
}

func TestOCI_PushBundle_MissingChunkInMap(t *testing.T) {
	_, _, c := startMock(t)
	ctx := context.Background()

	// Manifest references a layer not in the chunks map → push fails.
	manifest := &OCIManifest{
		SchemaVersion: 2,
		MediaType:     ociManifestMediaType,
		Config: OCIDescriptor{
			MediaType: cacheConfigMediaType,
			Digest:    digestFor(blake3.Sum256([]byte("cfg"))),
			Size:      3,
		},
		Layers: []OCIDescriptor{
			{
				MediaType: cacheLayerMediaType,
				Digest:    "sha256:" + strings.Repeat("a", 64),
				Size:      0,
			},
		},
	}
	_, err := c.PushBundle(ctx, manifest, []byte("cfg"), map[string][]byte{}, "", 2)
	if err == nil {
		t.Fatalf("PushBundle should fail on missing chunk in map; got nil")
	}
	if !strings.Contains(err.Error(), "missing chunk") {
		t.Errorf("expected 'missing chunk' in error; got %v", err)
	}
}

func TestOCI_PutBlob_HEAD500Surfaces(t *testing.T) {
	_, reg, c := startMock(t)
	ctx := context.Background()
	reg.mu.Lock()
	reg.failHEAD = true
	reg.mu.Unlock()
	err := c.PutBlob(ctx, digestFor(blake3.Sum256([]byte("x"))), []byte("x"))
	if err == nil {
		t.Fatalf("PutBlob should surface 500 on HEAD; got nil")
	}
	if !strings.Contains(err.Error(), "head before put") {
		t.Errorf("expected 'head before put' in error; got %v", err)
	}
}

func TestOCI_PutBlob_PUTFailureSurfaces(t *testing.T) {
	_, reg, c := startMock(t)
	ctx := context.Background()
	reg.mu.Lock()
	reg.failPUT = true
	reg.mu.Unlock()
	err := c.PutBlob(ctx, digestFor(blake3.Sum256([]byte("x"))), []byte("x"))
	if err == nil {
		t.Fatalf("PutBlob should surface 500 on PUT; got nil")
	}
}

func TestOCI_ParallelChunkUpload_Bounded(t *testing.T) {
	_, reg, c := startMock(t)
	ctx := context.Background()

	// Push 20 chunks with parallelism=4.
	chunks := map[string][]byte{}
	var layers []OCIDescriptor
	for i := 0; i < 20; i++ {
		body := []byte(fmt.Sprintf("parallel-%d", i))
		digest := digestFor(blake3.Sum256(body))
		chunks[digest] = body
		layers = append(layers, OCIDescriptor{
			MediaType: cacheLayerMediaType,
			Digest:    digest,
			Size:      int64(len(body)),
		})
	}
	configBytes := []byte("cfg")
	manifest := &OCIManifest{
		SchemaVersion: 2,
		MediaType:     ociManifestMediaType,
		Config: OCIDescriptor{
			MediaType: cacheConfigMediaType,
			Digest:    digestFor(blake3.Sum256(configBytes)),
			Size:      int64(len(configBytes)),
		},
		Layers: layers,
	}
	if _, err := c.PushBundle(ctx, manifest, configBytes, chunks, "", 4); err != nil {
		t.Fatalf("PushBundle: %v", err)
	}

	reg.mu.Lock()
	defer reg.mu.Unlock()
	// 20 chunks + 1 config = 21 blobs.
	if len(reg.blobs) != 21 {
		t.Errorf("blob count after parallel push: want 21, got %d", len(reg.blobs))
	}
}
