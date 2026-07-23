// OCI Distribution Spec client for build-cache/v1 transport
// (bead mache-aeb262 Phase 3, consumes cloister-bb168f spec).
//
// Implements the routes named in
// `cloister-spec/build-cache/v1/wire/{push,pull}-protocol.md`:
//
//   HEAD /v2/<producer>/<scope>/blobs/<digest>
//   POST /v2/<producer>/<scope>/blobs/uploads/
//   PUT  /v2/<producer>/<scope>/blobs/uploads/<uuid>?digest=<digest>
//   GET  /v2/<producer>/<scope>/blobs/<digest>
//   GET  /v2/<producer>/<scope>/manifests/<ref>
//   PUT  /v2/<producer>/<scope>/manifests/<ref>
//
// Digest encoding: per the spec, every digest is `sha256:<hex>` where
// <hex> is the BLAKE3 hash bytes. Documented as a deliberate misuse
// of the algorithm prefix; future v2 may register a `blake3:` prefix.
//
// What this DOES handle:
//   - Idempotent push (HEAD-check skips re-upload)
//   - Parallel chunk upload (one goroutine per chunk, bounded by
//     a semaphore so we don't open hundreds of connections)
//   - Verify-on-read for every pulled blob (BLAKE3 of body vs digest)
//   - Schema-mismatch refusal (manifest config.mediaType)
//   - Tag and digest references on pull
//
// What this DOES NOT handle (v1.x follow-ups):
//   - OCI bearer-token auth flow — caller supplies a token; OAuth2
//     dance is the registry's concern, not this client's
//   - Cross-region replication / fallback URLs
//   - OCI mount-blob (cross-repo dedup) — falls back to plain upload
//   - Retries with backoff — caller wraps; we surface the first error
//   - HTTP/2 connection reuse beyond Go's net/http default
//
// Single-flight discipline: every method takes context.Context; the
// caller decides timeouts + cancellation. No package-global state.

package cmd

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/zeebo/blake3"
)

// MediaType constants per cloister-spec/build-cache/v1/wire/manifest-shape.md.
const (
	ociManifestMediaType  = "application/vnd.oci.image.manifest.v1+json"
	cacheConfigMediaType  = "application/vnd.cloister.build-cache.v1.config+json"
	cacheLayerMediaType   = "application/vnd.cloister.build-cache.v1.chunk"
	uploadStreamMediaType = "application/octet-stream"
)

// OCIClient speaks the build-cache/v1 transport against a provider.
//
// Construct via NewOCIClient(baseURL). Methods are safe to call
// concurrently on the same client; the underlying http.Client handles
// connection pooling.
type OCIClient struct {
	baseURL  string // e.g. "https://cache.example.com" (no trailing slash)
	producer string // e.g. "mache"
	scope    string // e.g. "rosary/abc123def"
	http     *http.Client
	token    string // optional bearer token; empty = unauthenticated
}

// NewOCIClient builds a client targeting <baseURL>/v2/<producer>/<scope>/.
// Empty baseURL is rejected up front; producer + scope can be empty
// only if the caller is using methods that don't need them (we don't
// have any such methods today, but the constructor doesn't enforce
// non-empty to keep test ergonomics simple).
func NewOCIClient(baseURL, producer, scope string) (*OCIClient, error) {
	if baseURL == "" {
		return nil, errors.New("OCI client: baseURL is required")
	}
	// Strip trailing slash so URL assembly stays predictable.
	for len(baseURL) > 0 && baseURL[len(baseURL)-1] == '/' {
		baseURL = baseURL[:len(baseURL)-1]
	}
	if _, err := url.Parse(baseURL); err != nil {
		return nil, fmt.Errorf("OCI client: invalid baseURL %q: %w", baseURL, err)
	}
	return &OCIClient{
		baseURL:  baseURL,
		producer: producer,
		scope:    scope,
		http:     http.DefaultClient,
	}, nil
}

// SetToken configures bearer-token auth for subsequent requests.
// Empty token clears it.
func (c *OCIClient) SetToken(token string) {
	c.token = token
}

// SetHTTPClient swaps the underlying *http.Client — useful for tests
// (httptest) and for callers that need custom timeouts / TLS.
func (c *OCIClient) SetHTTPClient(client *http.Client) {
	if client != nil {
		c.http = client
	}
}

// digestURL returns "<base>/v2/<producer>/<scope>/blobs/<digest>".
func (c *OCIClient) digestURL(digest string) string {
	return fmt.Sprintf("%s/v2/%s/%s/blobs/%s",
		c.baseURL, c.producer, c.scope, digest)
}

// uploadsURL returns "<base>/v2/<producer>/<scope>/blobs/uploads/".
func (c *OCIClient) uploadsURL() string {
	return fmt.Sprintf("%s/v2/%s/%s/blobs/uploads/",
		c.baseURL, c.producer, c.scope)
}

// manifestURL returns "<base>/v2/<producer>/<scope>/manifests/<ref>".
func (c *OCIClient) manifestURL(ref string) string {
	return fmt.Sprintf("%s/v2/%s/%s/manifests/%s",
		c.baseURL, c.producer, c.scope, ref)
}

// digestFor formats BLAKE3 bytes as the v1 wire digest. The spec
// reuses `sha256:<hex>` as the prefix; documented in
// cloister-spec/build-cache/v1/README.md §"Digest encoding".
func digestFor(blake3Hash [32]byte) string {
	return "sha256:" + hex.EncodeToString(blake3Hash[:])
}

// attachAuth adds bearer-token header if set.
func (c *OCIClient) attachAuth(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

// ── HEAD blob (existence check) ───────────────────────────────────

// HeadBlob reports whether the registry has a blob at the given digest.
// Mapping: 200 → true, 404 → false, anything else → error.
//
// Per push-protocol.md step 1: producers HEAD before uploading to
// honor the idempotency contract (no re-upload of existing chunks).
func (c *OCIClient) HeadBlob(ctx context.Context, digest string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.digestURL(digest), nil)
	if err != nil {
		return false, err
	}
	c.attachAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("HEAD %s: %w", req.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, ociErrorFromResponse(resp)
	}
}

// ── PUT blob (upload chunk or config) ─────────────────────────────

// PutBlob uploads `data` under `digest`. Two-step per OCI:
//
//	POST /v2/<scope>/blobs/uploads/      → 202 Location: <upload-url>
//	PUT  <upload-url>?digest=<digest>    → 201 Created
//
// HEAD-check first: skip the upload if the registry already has it.
// Returns nil on success (idempotent — re-uploading an existing blob
// is a no-op from the producer's perspective).
func (c *OCIClient) PutBlob(ctx context.Context, digest string, data []byte) error {
	exists, err := c.HeadBlob(ctx, digest)
	if err != nil {
		return fmt.Errorf("head before put: %w", err)
	}
	if exists {
		return nil // (IM) axiom
	}

	// Step 1: initiate upload.
	initReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.uploadsURL(), nil)
	if err != nil {
		return err
	}
	c.attachAuth(initReq)
	initResp, err := c.http.Do(initReq)
	if err != nil {
		return fmt.Errorf("POST %s: %w", initReq.URL, err)
	}
	defer func() { _ = initResp.Body.Close() }()
	if initResp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("POST upload init: status %d: %w",
			initResp.StatusCode, ociErrorFromResponse(initResp))
	}
	uploadURL := initResp.Header.Get("Location")
	if uploadURL == "" {
		return fmt.Errorf("POST upload init: no Location header in 202 response")
	}
	// Some registries return a relative URL; resolve against baseURL.
	if !strings.HasPrefix(uploadURL, "http://") && !strings.HasPrefix(uploadURL, "https://") {
		uploadURL = c.baseURL + uploadURL
	}

	// Step 2: PUT the body with ?digest=<digest>.
	putURL := uploadURL
	if strings.Contains(uploadURL, "?") {
		putURL += "&digest=" + url.QueryEscape(digest)
	} else {
		putURL += "?digest=" + url.QueryEscape(digest)
	}

	putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, putURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	c.attachAuth(putReq)
	putReq.Header.Set("Content-Type", uploadStreamMediaType)
	putReq.ContentLength = int64(len(data))

	putResp, err := c.http.Do(putReq)
	if err != nil {
		return fmt.Errorf("PUT %s: %w", putURL, err)
	}
	defer func() { _ = putResp.Body.Close() }()
	if putResp.StatusCode != http.StatusCreated {
		return fmt.Errorf("PUT blob: status %d: %w",
			putResp.StatusCode, ociErrorFromResponse(putResp))
	}
	// Per push-protocol.md step 2 the registry returns a
	// Docker-Content-Digest header confirming the digest. If it
	// disagrees with ours, that's a registry bug; surface it.
	if confirmed := putResp.Header.Get("Docker-Content-Digest"); confirmed != "" && confirmed != digest {
		return fmt.Errorf("registry confirmed digest %q != requested %q (BLOB_DIGEST_MISMATCH per spec)",
			confirmed, digest)
	}
	return nil
}

// ── GET blob (with verify-on-read) ────────────────────────────────

// GetBlob fetches the bytes under `digest` and verifies BLAKE3(body)
// matches the digest. Mismatch is a hard error — the consumer MUST
// refuse to vouch for tampered/corrupted blobs (per LLO BlobStore
// substrate contract).
func (c *OCIClient) GetBlob(ctx context.Context, digest string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.digestURL(digest), nil)
	if err != nil {
		return nil, err
	}
	c.attachAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", req.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, &OCIBlobMissingError{Digest: digest}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET blob %s: status %d: %w",
			digest, resp.StatusCode, ociErrorFromResponse(resp))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read GET blob body: %w", err)
	}
	// Verify-on-read. Strip "sha256:" prefix and re-hash.
	expected, ok := strings.CutPrefix(digest, "sha256:")
	if !ok {
		return nil, fmt.Errorf("digest %q missing sha256: prefix (v1 wire encoding)", digest)
	}
	actual := blake3.Sum256(body)
	actualHex := hex.EncodeToString(actual[:])
	if actualHex != expected {
		return nil, fmt.Errorf("GET blob %s integrity violation: BLAKE3(body)=%s",
			digest, actualHex)
	}
	return body, nil
}

// ── PUT manifest ──────────────────────────────────────────────────

// OCIManifest is the JSON shape per
// cloister-spec/build-cache/v1/wire/manifest-shape.md. Used for both
// push (we build + serialize) and pull (we parse + verify).
type OCIManifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	Config        OCIDescriptor     `json:"config"`
	Layers        []OCIDescriptor   `json:"layers"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// OCIDescriptor matches OCI Image Spec descriptor shape.
type OCIDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// PutManifest uploads the manifest under <ref> (a digest like
// "sha256:abc..." or a tag like "latest").
//
// Returns the manifest's own digest (BLAKE3 of the JSON body) so
// producers that pushed by tag can also pin the immutable form.
func (c *OCIClient) PutManifest(ctx context.Context, ref string, manifest *OCIManifest) (string, error) {
	body, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("marshal manifest: %w", err)
	}
	manifestDigest := digestFor(blake3.Sum256(body))

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.manifestURL(ref), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	c.attachAuth(req)
	req.Header.Set("Content-Type", ociManifestMediaType)
	req.ContentLength = int64(len(body))

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("PUT manifest %s: %w", ref, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("PUT manifest %s: status %d: %w",
			ref, resp.StatusCode, ociErrorFromResponse(resp))
	}
	return manifestDigest, nil
}

// ── GET manifest ──────────────────────────────────────────────────

// GetManifest fetches and parses the manifest under <ref>. Verifies
// the body is well-formed OCI manifest with the expected mediaType +
// config.mediaType, and re-hashes the body to assert content-address
// integrity (if <ref> looks like a digest).
//
// Returns the parsed manifest + the manifest's actual BLAKE3 digest
// (useful for tag-resolved pulls: pin the digest from the response).
func (c *OCIClient) GetManifest(ctx context.Context, ref string) (*OCIManifest, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.manifestURL(ref), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", ociManifestMediaType)
	c.attachAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("GET manifest %s: %w", ref, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, "", &OCIManifestMissingError{Ref: ref}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("GET manifest %s: status %d: %w",
			ref, resp.StatusCode, ociErrorFromResponse(resp))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read manifest body: %w", err)
	}

	// Verify digest if <ref> looks like sha256:...
	actualDigest := digestFor(blake3.Sum256(body))
	if strings.HasPrefix(ref, "sha256:") && ref != actualDigest {
		return nil, "", fmt.Errorf("GET manifest %s integrity violation: BLAKE3(body)=%s",
			ref, actualDigest)
	}

	var m OCIManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, "", fmt.Errorf("parse manifest JSON: %w", err)
	}
	if m.MediaType != ociManifestMediaType {
		return nil, "", fmt.Errorf("manifest mediaType %q != %q",
			m.MediaType, ociManifestMediaType)
	}
	if m.Config.MediaType != cacheConfigMediaType {
		return nil, "", fmt.Errorf("manifest config.mediaType %q != %q (refusing non-build-cache artifact)",
			m.Config.MediaType, cacheConfigMediaType)
	}
	return &m, actualDigest, nil
}

// ── high-level: push a full bundle ────────────────────────────────

// PushBundle uploads a (config, chunks) bundle:
//   - PutBlob the config (idempotent)
//   - PutBlob every chunk in parallel (idempotent, bounded concurrency)
//   - PutManifest under both the manifest's content digest AND the
//     human-readable tag (if non-empty)
//
// Returns the manifest's content digest. Producers SHOULD pin to that
// digest for future pulls even when they also pushed a tag.
//
// chunks is a map digest → bytes. The manifest references those
// digests in its layers; if a layer digest isn't in the map, that's
// a producer bug and PushBundle fails.
//
// parallelism controls how many concurrent chunk uploads run. 0 → 4.
func (c *OCIClient) PushBundle(ctx context.Context, manifest *OCIManifest, configBytes []byte, chunks map[string][]byte, tag string, parallelism int) (string, error) {
	if parallelism <= 0 {
		parallelism = 4
	}

	// 1. Config blob first.
	if err := c.PutBlob(ctx, manifest.Config.Digest, configBytes); err != nil {
		return "", fmt.Errorf("push config: %w", err)
	}

	// 2. Chunks in parallel, bounded.
	type chunkErr struct {
		digest string
		err    error
	}
	errs := make(chan chunkErr, len(manifest.Layers))
	sem := make(chan struct{}, parallelism)
	var wg sync.WaitGroup
	for _, layer := range manifest.Layers {
		body, ok := chunks[layer.Digest]
		if !ok {
			return "", fmt.Errorf("PushBundle: missing chunk for layer %s in chunks map", layer.Digest)
		}
		layerCopy := layer
		bodyCopy := body
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := ctx.Err(); err != nil {
				errs <- chunkErr{layerCopy.Digest, err}
				return
			}
			if err := c.PutBlob(ctx, layerCopy.Digest, bodyCopy); err != nil {
				errs <- chunkErr{layerCopy.Digest, err}
			}
		}()
	}
	wg.Wait()
	close(errs)
	var multi []string
	for e := range errs {
		multi = append(multi, fmt.Sprintf("chunk %s: %v", e.digest, e.err))
	}
	if len(multi) > 0 {
		return "", fmt.Errorf("PushBundle: %d chunk error(s): %s",
			len(multi), strings.Join(multi, "; "))
	}

	// 3. Manifest by digest (immutable canonical reference).
	body, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("marshal manifest for digest: %w", err)
	}
	manifestDigest := digestFor(blake3.Sum256(body))
	if _, err := c.PutManifest(ctx, manifestDigest, manifest); err != nil {
		return "", fmt.Errorf("push manifest by digest: %w", err)
	}

	// 4. Manifest by tag (optional, mutable alias).
	if tag != "" {
		if _, err := c.PutManifest(ctx, tag, manifest); err != nil {
			return "", fmt.Errorf("push manifest by tag %q: %w", tag, err)
		}
	}

	return manifestDigest, nil
}

// ── high-level: pull a full bundle ────────────────────────────────

// PullBundle is the inverse of PushBundle: GET manifest, GET config,
// GET every layer, verify-on-read at every step.
//
// Returns (manifest, configBytes, chunks). chunks is a map digest → bytes.
//
// Caller is responsible for asserting the lockfile's internal
// invariants (chunk-hash chain, root). This client just delivers the
// bytes faithfully.
func (c *OCIClient) PullBundle(ctx context.Context, ref string, parallelism int) (*OCIManifest, []byte, map[string][]byte, string, error) {
	if parallelism <= 0 {
		parallelism = 4
	}

	// 1. Manifest
	manifest, manifestDigest, err := c.GetManifest(ctx, ref)
	if err != nil {
		return nil, nil, nil, "", err
	}

	// 2. Config blob (sequential — small + needed before chunks)
	configBytes, err := c.GetBlob(ctx, manifest.Config.Digest)
	if err != nil {
		return nil, nil, nil, "", fmt.Errorf("pull config: %w", err)
	}

	// 3. Chunks in parallel
	chunks := make(map[string][]byte, len(manifest.Layers))
	var mu sync.Mutex
	type chunkErr struct {
		digest string
		err    error
	}
	errs := make(chan chunkErr, len(manifest.Layers))
	sem := make(chan struct{}, parallelism)
	var wg sync.WaitGroup
	for _, layer := range manifest.Layers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := ctx.Err(); err != nil {
				errs <- chunkErr{layer.Digest, err}
				return
			}
			body, err := c.GetBlob(ctx, layer.Digest)
			if err != nil {
				errs <- chunkErr{layer.Digest, err}
				return
			}
			mu.Lock()
			chunks[layer.Digest] = body
			mu.Unlock()
		}()
	}
	wg.Wait()
	close(errs)
	var multi []string
	for e := range errs {
		multi = append(multi, fmt.Sprintf("chunk %s: %v", e.digest, e.err))
	}
	if len(multi) > 0 {
		return nil, nil, nil, "", fmt.Errorf("PullBundle: %d chunk error(s): %s",
			len(multi), strings.Join(multi, "; "))
	}

	return manifest, configBytes, chunks, manifestDigest, nil
}

// ── error types ───────────────────────────────────────────────────

// OCIBlobMissingError is returned by GetBlob on 404. Callers can
// errors.As() it to distinguish "blob not present" from other errors.
type OCIBlobMissingError struct {
	Digest string
}

func (e *OCIBlobMissingError) Error() string {
	return fmt.Sprintf("OCI blob missing: %s", e.Digest)
}

// OCIManifestMissingError is returned by GetManifest on 404.
type OCIManifestMissingError struct {
	Ref string
}

func (e *OCIManifestMissingError) Error() string {
	return fmt.Sprintf("OCI manifest missing: %s", e.Ref)
}

// ociErrorEnvelope is the OCI Distribution Spec error shape.
type ociErrorEnvelope struct {
	Errors []ociErrorEntry `json:"errors"`
}

type ociErrorEntry struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Detail  json.RawMessage `json:"detail,omitempty"`
}

// ociErrorFromResponse builds a descriptive error from a non-2xx HTTP
// response. Tries to parse the OCI errors envelope; falls back to the
// raw body if parsing fails.
func ociErrorFromResponse(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var env ociErrorEnvelope
	if err := json.Unmarshal(body, &env); err == nil && len(env.Errors) > 0 {
		first := env.Errors[0]
		return fmt.Errorf("OCI %d %s: %s (code=%s)",
			resp.StatusCode, resp.Status, first.Message, first.Code)
	}
	return fmt.Errorf("OCI %d %s: %s",
		resp.StatusCode, resp.Status, strings.TrimSpace(string(body)))
}
