# mache OCI Distribution Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** mache self-publishes a leyline-bundled multi-arch OCI image to ghcr on release, declares that image in its own `server.json` (`packages[].oci`), and fixes the stale find_smells rule count in docs.

**Architecture:** Port the archived `kiln` repo's proven image recipe (Dockerfile.release + buildx multi-arch + cosign) into mache's own `release.yml`. Emit `packages[].oci` from the existing `server-json-gen` (version single-sourced from `buildinfo.Version`). Docs pass fixes the rule count.

**Tech Stack:** GitHub Actions (docker/build-push-action, cosign), Docker (`debian:bookworm-slim`), Go (server-json-gen), testify.

## Global Constraints

- **leyline dynamically links libsqlite3** → the bundled image is `debian:bookworm-slim` + `libsqlite3-0`, NOT distroless.
- **Every GitHub Actions `uses:` ref MUST be 40-hex SHA-pinned** with a `# vX` comment (`scripts/actions-pin-lint.sh`; `TestActionsPinLint_RepoIsClean`; CI `task actions:lint`).
- **`packages[]` is tag-pinned only** (`identifier:version`); digest/CAS + provider choice are the consumer's (cloister's) concern — out of scope.
- **Version single-sourced:** `packages[].version` = `buildinfo.Version` (← `version.txt`); never a literal.
- Registry: `ghcr.io/agentic-research/mache`; pushed via `GITHUB_TOKEN` (`packages: write`), signed via cosign keyless (`id-token: write`).
- `gen:server-json:check` is a CI gate — regenerate `server.json` after any generator change.
- Formatter: **gofumpt** (`task fmt`); tests **testify**; pre-commit hooks reformat docs via mdformat (re-add + re-commit).
- **No co-author lines** in commits; stage paths explicitly (never `git add -A`).
- Branch: `mache-oci-distribution` (already checked out).

______________________________________________________________________

### Task 1: `packages[].oci` in server.json (C2)

**Files:**

- Modify: `tools/server-json-gen/main.go` (add `packageEntry` type; `Packages` field on `serverDoc` ~line 90–99; set it in `buildDoc` ~line 278; add an identifier const)
- Test: `tools/server-json-gen/main_test.go` (new)
- Regenerate: `server.json`

**Interfaces:**

- Consumes: `buildinfo.Version` (already imported as `serverVersion`); `buildDoc() (*serverDoc, error)`.

- Produces: `server.json` gains a top-level `packages` array with one `oci` entry.

- [ ] **Step 1: Write the failing test**

Create `tools/server-json-gen/main_test.go`:

```go
package main

import "testing"

// TestBuildDoc_EmitsOCIPackage asserts server.json declares its own OCI
// image source (ghcr) tag-pinned to the build version, so cloister (ADR-0038)
// can derive the bundle image instead of a hand-written, drift-prone tag.
func TestBuildDoc_EmitsOCIPackage(t *testing.T) {
	doc, err := buildDoc()
	if err != nil {
		t.Fatalf("buildDoc: %v", err)
	}
	if len(doc.Packages) != 1 {
		t.Fatalf("want exactly 1 package, got %d", len(doc.Packages))
	}
	p := doc.Packages[0]
	if p.RegistryType != "oci" {
		t.Errorf("registryType = %q, want \"oci\"", p.RegistryType)
	}
	if p.Identifier != "ghcr.io/agentic-research/mache" {
		t.Errorf("identifier = %q, want ghcr.io/agentic-research/mache", p.Identifier)
	}
	if p.Version != serverVersion {
		t.Errorf("version = %q, want serverVersion %q (single-sourced from buildinfo)", p.Version, serverVersion)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./tools/server-json-gen/ -run TestBuildDoc_EmitsOCIPackage -v`
Expected: FAIL — `doc.Packages` undefined (compile error: `serverDoc` has no field `Packages`).

- [ ] **Step 3: Add the `packageEntry` type + `Packages` field + identifier const**

In `tools/server-json-gen/main.go`, add the identifier const near the other `server*` consts (after `serverName`):

```go
// ociImageIdentifier is the registry path mache's release publishes its
// leyline-bundled OCI image to (see .github/workflows/release.yml image job).
// Declared in server.json packages[] so consumers (cloister ADR-0038) derive
// the bundle image from the version-pinned artifact instead of a hand-written
// tag. Tag-pinned only — digest/CAS pinning is the consumer's concern.
const ociImageIdentifier = "ghcr.io/agentic-research/mache"
```

Add the `packageEntry` type next to the other wire structs (e.g. after `repository`):

```go
type packageEntry struct {
	RegistryType string `json:"registryType"`
	Identifier   string `json:"identifier"`
	Version      string `json:"version"`
}
```

Add the field to `serverDoc` between `WebsiteURL` and `Remotes` (MCP schema places `packages` before `remotes`):

```go
	WebsiteURL  string         `json:"websiteUrl"`
	Packages    []packageEntry `json:"packages"`
	Remotes     []remote       `json:"remotes"`
```

- [ ] **Step 4: Populate `Packages` in `buildDoc`**

In `buildDoc`, in the `doc := &serverDoc{...}` literal, add the `Packages` field right after `WebsiteURL: websiteURL,`:

```go
		WebsiteURL: websiteURL,
		Packages: []packageEntry{{
			RegistryType: "oci",
			Identifier:   ociImageIdentifier,
			Version:      serverVersion,
		}},
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./tools/server-json-gen/ -run TestBuildDoc_EmitsOCIPackage -v`
Expected: PASS.

- [ ] **Step 6: Regenerate server.json + verify the drift gate is green**

Run: `task gen:server-json && task gen:server-json:check`
Expected: `server.json is up to date`. Confirm `server.json` now has a `"packages"` block with the oci entry at version = current `version.txt`.

Run: `go build ./... && task fmt`
Expected: clean.

- [ ] **Step 7: Commit**

```bash
git add tools/server-json-gen/main.go tools/server-json-gen/main_test.go server.json
git commit -m "feat(server-json): declare OCI image source in packages[] (mache OCI distribution C2)"
```

______________________________________________________________________

### Task 2: `Dockerfile.release` — leyline-bundled runtime image (C1a)

**Files:**

- Create: `Dockerfile.release`

**Interfaces:**

- Consumes: build-context dirs `image/linux-<arch>/{mache,leyline}` (populated by the release job, Task 3).

- Produces: a runnable image with `mache` + `leyline` on PATH; `ENTRYPOINT ["mache","serve"]`.

- [ ] **Step 1: Create `Dockerfile.release`**

Create `Dockerfile.release` at the repo root (adapted from kiln's; tooling-native entrypoint, no wrapper script):

```dockerfile
# Dockerfile.release — mache's published multi-arch runtime image.
#
# NOT distroless: the bundled leyline (Rust) binary dynamically links
# libsqlite3, so the base carries libsqlite3-0 + ca-certificates. mache's
# own distroless apko.yaml / `task image` path is a separate, mache-only
# local-dev image and is intentionally kept alongside this.
#
# Built by the release workflow's `image` job from pre-compiled binaries:
#   image/linux-${TARGETARCH}/mache      (release matrix artifact)
#   image/linux-${TARGETARCH}/leyline    (pinned ley-line-open binary)
# Multi-arch via buildx; the correct per-arch binaries are copied in.
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates libsqlite3-0 \
    && rm -rf /var/lib/apt/lists/*

# Non-root, matches the distroless apko uid/gid.
RUN groupadd -g 65532 mache && useradd -u 65532 -g mache -s /usr/sbin/nologin mache

ARG TARGETARCH
COPY image/linux-${TARGETARCH}/mache /usr/local/bin/mache
COPY image/linux-${TARGETARCH}/leyline /usr/local/bin/leyline
RUN chmod +x /usr/local/bin/mache /usr/local/bin/leyline

USER mache

# mache's own state dir (leyline cache + projection cache) and the tree to
# project. Configure via a mounted .mache.json + MACHE_* env — no wrapper.
ENV HOME=/data
VOLUME /data
VOLUME /source

# Tooling-native: run mache directly. leyline is on PATH → LookPath finds it
# → LLO in-container with zero runtime download.
ENTRYPOINT ["/usr/local/bin/mache", "serve"]
CMD ["/source"]
```

- [ ] **Step 2: Verify the Dockerfile builds with placeholder binaries**

Run (creates throwaway stand-in binaries so the COPY/build succeeds without the real release artifacts):

```bash
mkdir -p image/linux-amd64
printf '#!/bin/sh\necho stub\n' > image/linux-amd64/mache
printf '#!/bin/sh\necho stub\n' > image/linux-amd64/leyline
docker build -f Dockerfile.release --build-arg TARGETARCH=amd64 -t mache-release-smoke .
docker run --rm --entrypoint /usr/local/bin/leyline mache-release-smoke
rm -rf image/
```

Expected: build succeeds; the run prints `stub` (leyline present + executable on PATH). If Docker is unavailable in this environment, run `hadolint Dockerfile.release` instead (or note that CI/the release job is the real validator) — do NOT skip silently.

- [ ] **Step 3: Commit**

```bash
git add Dockerfile.release
git commit -m "feat(image): Dockerfile.release — leyline-bundled runtime image (mache OCI distribution C1a)"
```

______________________________________________________________________

### Task 3: release.yml `image` job — build + push + sign (C1b)

**Files:**

- Modify: `.github/workflows/release.yml` (add an `image` job after `build`)

**Interfaces:**

- Consumes: the `build` job's `mache-linux-amd64` / `mache-linux-arm64` artifacts; `Dockerfile.release` (Task 2); the `leylineBinaryVersion` const in `internal/leyline/socket.go`.

- Produces: `ghcr.io/agentic-research/mache:${TAG}` + `:latest`, cosign-signed.

- [ ] **Step 1: Resolve the SHA pins for the new actions**

Every new `uses:` must be a 40-hex commit SHA. Resolve each (dereference annotated tags to the commit):

```bash
for repo in docker/login-action docker/setup-buildx-action docker/build-push-action sigstore/cosign-installer; do
  sha=$(gh api repos/$repo/git/refs/tags/$(gh api repos/$repo/releases/latest --jq .tag_name) --jq '.object.sha')
  # if the ref is an annotated tag object, deref to the commit:
  csha=$(gh api repos/$repo/git/tags/$sha --jq '.object.sha' 2>/dev/null || echo "$sha")
  echo "$repo  ->  $csha"
done
# actions/download-artifact is already SHA-pinned elsewhere in this repo — reuse that exact pin:
grep -h 'download-artifact@' .github/workflows/release.yml
```

Record each `owner/action@<40-hex> # vMAJOR`.

- [ ] **Step 2: Add the `image` job**

In `.github/workflows/release.yml`, add a new job after the `build` job (substitute the `<...SHA...>` placeholders with the values from Step 1; the `# vX` comment is required by pin-lint):

```yaml
  image:
    needs: build
    runs-on: ubuntu-latest
    permissions:
      contents: read
      packages: write      # push to ghcr
      id-token: write       # cosign keyless + SLSA provenance
    steps:
      - uses: actions/checkout@<CHECKOUT_SHA> # v7.0.0

      - name: Read pinned leyline version
        id: ll
        run: |
          set -euo pipefail
          # Single source of truth: the const mache already downloads by.
          v=$(grep -oE 'leylineBinaryVersion = "v[0-9.]+"' internal/leyline/socket.go | grep -oE 'v[0-9.]+')
          echo "version=$v" >> "$GITHUB_OUTPUT"

      - name: Download mache linux binaries
        uses: actions/download-artifact@<DOWNLOAD_ARTIFACT_SHA> # v8
        with:
          pattern: mache-linux-*
          path: artifacts

      - name: Assemble per-arch build context
        env:
          LL_VERSION: ${{ steps.ll.outputs.version }}
        run: |
          set -euo pipefail
          mkdir -p image/linux-amd64 image/linux-arm64
          mv artifacts/mache-linux-amd64/mache-linux-amd64 image/linux-amd64/mache
          mv artifacts/mache-linux-arm64/mache-linux-arm64 image/linux-arm64/mache
          for arch in amd64 arm64; do
            curl -sSfL "https://github.com/agentic-research/ley-line-open/releases/download/${LL_VERSION}/leyline-linux-${arch}" \
              -o "image/linux-${arch}/leyline"
          done
          chmod +x image/linux-*/mache image/linux-*/leyline

      - name: Log in to ghcr
        uses: docker/login-action@<LOGIN_SHA> # v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}

      - name: Set up Buildx
        uses: docker/setup-buildx-action@<BUILDX_SHA> # v3

      - name: Build and push
        id: push
        uses: docker/build-push-action@<BUILDPUSH_SHA> # v6
        with:
          context: .
          file: Dockerfile.release
          push: true
          platforms: linux/amd64,linux/arm64
          provenance: true
          tags: |
            ghcr.io/agentic-research/mache:${{ env.TAG }}
            ghcr.io/agentic-research/mache:latest

      - name: Install cosign
        uses: sigstore/cosign-installer@<COSIGN_SHA> # v3

      - name: Sign the pushed image
        env:
          DIGEST: ${{ steps.push.outputs.digest }}
        run: cosign sign --yes "ghcr.io/agentic-research/mache@${DIGEST}"
```

Notes: `env.TAG` is already defined at workflow level in `release.yml` (used by the release-honesty gate). `download-artifact@v8` nests each artifact under `artifacts/<name>/<name>` — hence the `mv artifacts/mache-linux-amd64/mache-linux-amd64 …`. If the build job uploads with a different internal filename, adjust the `mv` source to match `${{ matrix.artifact }}`.

- [ ] **Step 3: Validate the workflow (pin-lint + actionlint)**

Run: `task actions:lint`
Expected: `all workflow uses: refs are SHA-pinned ✓` (fails if any `<...SHA...>` placeholder was left unresolved).

Run: `go test -run 'TestActionsPinLint_RepoIsClean' ./cmd/`
Expected: PASS.

Run (if `actionlint` is available): `actionlint .github/workflows/release.yml` → no errors. If unavailable, note it; CI runs it.

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/release.yml
git commit -m "feat(release): publish leyline-bundled multi-arch ghcr image + cosign (mache OCI distribution C1b)"
```

______________________________________________________________________

### Task 4: Docs — rule count + OCI distribution note (C3)

**Files:**

- Modify: `docs/ROADMAP.md`, `docs/competitive-landscape-2026.md`, `docs/PRIOR_ART.md` (rule count), `docs/ARCHITECTURE.md` + `README.md` (OCI note)

**Interfaces:** none (docs only).

- [ ] **Step 1: Confirm the authoritative rule count**

Run:

```bash
grep -cE '^\s*ID:\s*"[a-z_]+"' cmd/smell_rules.go
grep -oE '^\s*ID:\s*"[a-z_]+"' cmd/smell_rules.go | sed -E 's/.*"([a-z_]+)".*/\1/' | sort
```

Expected: a count (14 as of this writing) + the rule list. Use the **actual** number the command prints.

- [ ] **Step 2: Fix the stale count in the three docs**

Edit each occurrence of the stale "9 rules" framing to the real count, framed by family so it stays honest:

- `docs/ROADMAP.md`: change "9 structural rules (`magic_int_in_comparison`, …)" to the real count. Frame: "N find_smells rules — M code-structure (dead_code, god_file, long_function, cyclomatic_complexity, duplicate_code, duplicate_definitions, fan_out_skew, long_file, untested_function, magic_int_in_comparison), 3 doc-drift (drift_doc_broken_internal_link, drift_doc_dead_symbol_reference, drift_doc_outdated_count), 1 test (sleep_in_test)". Use the exact list from Step 1.

- `docs/competitive-landscape-2026.md`: fix "find_smells (9 rules)" (line ~47) and "four of the nine find_smells rules" (line ~27) — the `_ast`-dependent subset is now magic_int_in_comparison, cyclomatic_complexity, long_function, long_file, duplicate_code (confirm against `Requires: []string{"_ast"…}` in `cmd/smell_rules.go`).

- `docs/PRIOR_ART.md`: "parts of `find_smells`" (line ~19) — align to the same family framing.

- [ ] **Step 3: Add the OCI distribution note**

In `docs/ARCHITECTURE.md` (and a shorter mention in `README.md`), add a paragraph: mache self-publishes a leyline-bundled multi-arch image to `ghcr.io/agentic-research/mache` on release (debian-slim + libsqlite3 because leyline links sqlite; distroless apko stays for local dev), containers get LLO with no runtime fetch, and mache declares its own source via `server.json packages[].oci` (tag-pinned; digest/CAS is the consumer's concern).

- [ ] **Step 4: Keep docs:lint green**

If a release will cut alongside this work, bump `covers-version` on the touched docs to the new version (docs:lint gate). Otherwise leave markers.

Run: `task docs:lint`
Expected: `0 fail`.

- [ ] **Step 5: Commit**

```bash
git add docs/ROADMAP.md docs/competitive-landscape-2026.md docs/PRIOR_ART.md docs/ARCHITECTURE.md README.md
git commit -m "docs: fix find_smells rule count + OCI distribution note (mache-2c33b6, mache OCI distribution C3)"
```

______________________________________________________________________

### Task 5: Full validation + PR

- [ ] **Step 1: Run the full gate**

Run: `task check`
Expected: PASS (fmt + vet + lint + test + validate, incl. `gen:server-json:check`, `docs:lint`, `actions:lint`). Ignore pre-existing FUSE `-rpath` ld warnings.

- [ ] **Step 2: Push + open PR**

```bash
git push -u origin mache-oci-distribution
gh pr create --title "feat: mache OCI distribution — publish leyline-bundled ghcr image + declare packages[].oci" \
  --body "Self-publishes a leyline-bundled multi-arch ghcr image on release (Dockerfile.release ported from archived kiln; debian-slim + libsqlite3 since leyline links sqlite), declares packages[].oci in server.json (tag-pinned, version single-sourced), fixes the find_smells rule count. Spec: docs/superpowers/specs/2026-07-04-mache-oci-distribution-design.md. The image job only runs on a release tag; first published image + packages[]-bearing server.json land on the next release."
```

- [ ] **Step 3: Bead bookkeeping**

Comment on `mache-33dc5f` (image bundle now in-repo via Dockerfile.release + release job) and `mache-2c33b6` (rule count fixed) with the PR URL. Don't close until merged + a release actually publishes the image (the packages[].oci reference isn't live until then).

## Self-review notes

- Spec coverage: C1a=Task 2, C1b=Task 3, C2=Task 1, C3=Task 4, sequencing+validation=Task 5. Storage-class/CAS boundary is spec-documented + enforced by "tag-pinned only" in Task 1 (no digest logic). ✓
- The image job's real end-to-end validation only happens on an actual release tag (can't push a real image from a PR) — Task 3 validates structure (pin-lint/actionlint) + Task 2 validates the Dockerfile builds; the first release is the live proof. This is inherent to release infra, not a plan gap.
