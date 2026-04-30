# ART Platform Release Infrastructure

**Date**: 2026-03-24
**Status**: Draft
**Scope**: Cross-repo — affects `.github`, `rig`, `kiln`, `mache`, `ley-line`, `rosary`, `signet`

## Problem

The ART stack (mache, ley-line, rosary, signet) has fragmented release infrastructure:

- Release workflows exist in individual repos but are copy-pasted, inconsistent, and partially broken
- Merges to main cascade to kiln immediately with no release gate
- No shared CI (testing, linting, pre-commit) — each repo reinvents it
- GitHub repo settings (branch protection, secrets, dependabot) are manually configured per repo
- Signet commit signing and artifact signing exist in code but are never activated
- No trust chain from commit → build → artifact → distribution
- Copilot PR reviews burn credits on external contributor PRs

## Architecture

Three layers, clean separation of concerns:

```
agentic-research/.github (public)     rig/tofu/modules/github (private)
├── Reusable workflows                ├── Repo settings (TF)
│   ├── go-ci.yml                     ├── Branch protection
│   ├── rust-ci.yml                   ├── Secrets & variables
│   ├── go-release.yml                ├── Dependabot config
│   ├── rust-release.yml              ├── Copilot review rules
│   ├── pre-commit.yml                └── Required status checks
│   ├── signet-resign.yml
│   └── signet-sign.yml               kiln (public)
├── .pre-commit-config.yaml            ├── Downloads upstream artifacts
└── README.md                          ├── Links Go + Rust backends
                                       │   ├── mache + ley-line (staticlib)
                                       │   └── signet + signet-sign (wasm)
                                       ├── Packages all 4 tools
                                       ├── Publishes to homebrew-tap
                                       ├── Builds OCI images (apko/krust)
                                       └── Signs artifacts (signet + cosign)
```

### Data Flow

```
repo tag (v*)
  → shared workflow builds + tests
  → GH Release (binaries + checksums)
  → signet sign (artifact signatures)
  → repository_dispatch to kiln
      → kiln downloads artifacts
      → kiln links Rust backends (where applicable)
      → kiln publishes:
          - Homebrew tap (Formula/mache.rb, Formula/signet.rb, etc.)
          - OCI images (apko for Go, krust for Rust)
          - cosign signatures (sigstore transparency log)
      → signet-resign re-signs merge commit
```

## Component Design

### 1. Reusable Workflows (`agentic-research/.github`)

All workflows live in `.github/workflows/` in the org-level `.github` repo.
Consuming repos call them with one-liner workflow files.

#### `go-ci.yml` — Go CI (mache, signet)

```yaml
# Caller interface
on:
  workflow_call:
    inputs:
      go-version-file:
        type: string
        default: 'go.mod'  # reads toolchain directive, not a hardcoded version
      cgo:
        type: boolean
        default: false
      task-test-cmd:
        type: string
        default: 'task test'
      task-lint-cmd:
        type: string
        default: 'task lint'
      platforms:
        type: string
        default: '["ubuntu-latest", "macos-latest"]'
```

Steps:

- Install Go + Task
- If `cgo`: install platform FUSE deps (fuse-t on macOS, libfuse-dev on Linux)
- `task fmt` → check no diff
- `task vet`
- `task lint` (golangci-lint)
- `task test` (with `-race`)

#### `rust-ci.yml` — Rust CI (ley-line, rosary)

```yaml
on:
  workflow_call:
    inputs:
      rust-version:
        type: string
        default: 'stable'
      features:
        type: string
        default: ''
      platforms:
        type: string
        default: '["ubuntu-latest"]'
      cargo-test-args:
        type: string
        default: ''
```

Steps:

- Install Rust toolchain + Task
- Platform deps (libfuse3-dev on Linux, capnp if needed)
- `task fmt` (proxies to cargo fmt, consistent with Go path)
- `task lint` (proxies to cargo clippy)
- `task test`

Note: All steps use `task` commands, never raw `cargo`. Repos must have a
Taskfile.yml that wraps cargo commands with appropriate env/flags.

#### `go-release.yml` — Go Release (mache, signet)

```yaml
on:
  workflow_call:
    inputs:
      platforms:
        type: string
        default: '["darwin-arm64", "linux-amd64", "linux-arm64"]'
      cgo:
        type: boolean
        default: false
      dispatch-to:
        type: string
        default: '[]'  # JSON array of "owner/repo" strings
      binary-name:
        type: string
        required: true
    secrets:
      LEYLINE_PAT:
        required: false
      SIGNET_MASTER_KEY:
        required: false
```

Steps:

- Build matrix (per platform, maps to GHA runners: macos-latest, ubuntu-latest, ubuntu-24.04-arm)
- CGO setup when `cgo: true`
- `task release` (Taskfile wraps go build + ldflags + codesigning)
- Collect artifacts + SHA256 checksums
- If `SIGNET_MASTER_KEY` is set: download signet binary, `signet sign checksums-sha256.txt`
- Create GH Release with all artifacts + `.sig` files
- `repository_dispatch` to each repo in `dispatch-to`

Note: All build steps use `task` commands, never raw `go build`. The Taskfile
handles CGO flags, ldflags version injection, and macOS codesigning.

#### `rust-release.yml` — Rust Release (ley-line, rosary)

```yaml
on:
  workflow_call:
    inputs:
      platforms:
        type: string
        default: '["darwin-arm64", "linux-amd64", "linux-arm64"]'
      staticlib:
        type: boolean
        default: false
      cbindgen-header:
        type: boolean
        default: false
      wasm:
        type: boolean
        default: false
      crate:
        type: string
        required: true
      features:
        type: string
        default: ''
      dispatch-to:
        type: string
        default: '[]'
    secrets:
      SIGNET_MASTER_KEY:
        required: false
```

Steps:

- Cargo build matrix (per platform, `cargo-zigbuild` for cross-compile)
- If `staticlib`: produce `.a` + optional `.h` (cbindgen)
- If `wasm`: build for `wasm32-unknown-unknown`
- Collect artifacts + SHA256 checksums
- If `SIGNET_MASTER_KEY`: `signet sign checksums-sha256.txt`
- Create GH Release
- `repository_dispatch` to downstream

#### `pre-commit.yml` — Shared Pre-commit

```yaml
on:
  workflow_call:
    inputs:
      config-source:
        type: string
        default: 'remote'  # 'remote' uses .github repo config, 'local' uses repo's own
```

Steps:

- Checkout
- If `remote`: fetch `.pre-commit-config.yaml` from `agentic-research/.github`
- `pre-commit run --all-files`

The shared `.pre-commit-config.yaml` in `.github` includes:

- gofumpt (Go repos, gated with `types: [go]` so it skips Rust repos)
- rustfmt (Rust repos, gated with `types: [rust]` so it skips Go repos)
- File hygiene (trailing whitespace, large files, merge conflicts)
- Markdown formatting (mdformat)
- YAML/JSON validation
- gitleaks (secret detection)

Language-specific hooks use pre-commit's `types:` or `types_or:` filtering so
the shared config works in any repo without errors — Go hooks skip in Rust repos
and vice versa. Repos that need additional hooks beyond the shared set can use
`config-source: local` with a local `.pre-commit-config.yaml` that imports
the shared config via `default_install_hook_types`.

#### `signet-resign.yml` — Post-Merge Commit Re-signing

```yaml
on:
  workflow_call:
    secrets:
      SIGNET_MASTER_KEY:
        required: true
```

Steps:

- Guard: `if: ${{ vars.SIGNET_RESIGN_ENABLED == 'true' }}`
- Build signet from source (or download from GH Release)
- `signet authority start` with master key from secret
- Exchange GHA OIDC token for Ed25519 bridge cert
- Configure `git config gpg.x509.program signet-git`
- Amend HEAD commit with signet signature (preserve author/committer)
- `git push --force-with-lease` using a GitHub App token or PAT (not GITHUB_TOKEN)

Note: GITHUB_TOKEN cannot force-push to protected branches even with
`enforce_admins: false`. The resign workflow requires a token with branch
protection bypass rights. TF wires this as a repo secret (`RESIGN_TOKEN`)
generated from a GitHub App installation. GITHUB_TOKEN's inability to
retrigger workflows still provides loop prevention — the App token push
will trigger workflows but the `SIGNET_RESIGN_ENABLED` guard + concurrency
lock prevent infinite loops.

#### `copilot-review.yml` — Gated Copilot PR Review

```yaml
on:
  workflow_call:
    # No inputs — uses github.event context
```

Steps:

- Check `github.event.pull_request.author_association`
- If `OWNER`, `MEMBER`, or `COLLABORATOR`: request Copilot review
- If `FIRST_TIMER`, `CONTRIBUTOR`, `NONE`: skip (don't burn credits)

### 2. Consuming Repo Workflow Files

Each repo has minimal workflow files that call the shared ones.

**Example: `mache/.github/workflows/ci.yml`**

```yaml
name: CI
on:
  push:
    branches: [main]
    paths-ignore: ['docs/**', '*.md', 'LICENSE']
  pull_request:
    paths-ignore: ['docs/**', '*.md', 'LICENSE']

jobs:
  ci:
    uses: agentic-research/.github/.github/workflows/go-ci.yml@main
    with:
      cgo: true
      task-test-cmd: 'task test'
  pre-commit:
    uses: agentic-research/.github/.github/workflows/pre-commit.yml@main
  copilot:
    if: ${{ github.event_name == 'pull_request' }}
    uses: agentic-research/.github/.github/workflows/copilot-review.yml@main
```

**Example: `mache/.github/workflows/release.yml`**

```yaml
name: Release
on:
  push:
    tags: ['v*']

jobs:
  release:
    uses: agentic-research/.github/.github/workflows/go-release.yml@main
    with:
      binary-name: mache
      cgo: true
      dispatch-to: '["agentic-research/kiln"]'
    secrets: inherit
  resign:
    needs: release
    uses: agentic-research/.github/.github/workflows/signet-resign.yml@main
    secrets: inherit
```

**Example: `ley-line/.github/workflows/release.yml`**

```yaml
name: Release
on:
  push:
    tags: ['v*']

jobs:
  release:
    uses: agentic-research/.github/.github/workflows/rust-release.yml@main
    with:
      crate: leyline-cli
      features: 'lsp,embed'
      staticlib: true
      cbindgen-header: true
      wasm: false
      dispatch-to: '["agentic-research/kiln"]'  # kiln only — kiln pulls latest mache
    secrets: inherit
```

### 3. Kiln — Distribution Hub

Kiln receives `repository_dispatch` events from all four repos and produces
publishable artifacts.

#### Dispatch Matrix

| Source Event      | Kiln Action                                                        |
| ----------------- | ------------------------------------------------------------------ |
| `mache-release`   | Download mache + ley-line → link with `-tags leyline` → package    |
| `leyline-release` | Download ley-line + latest mache → rebuild linked binary → package |
| `signet-release`  | Download signet + signet-sign → link WASM backend → package        |
| `rosary-release`  | Download rosary binary → package                                   |

#### Build Outputs Per Tool

| Tool     | Homebrew            | OCI (apko)              | OCI (krust)       | Signed          |
| -------- | ------------------- | ----------------------- | ----------------- | --------------- |
| mache    | `Formula/mache.rb`  | Yes (Go + LL staticlib) | —                 | signet + cosign |
| signet   | `Formula/signet.rb` | Yes (Go + WASM)         | —                 | signet + cosign |
| ley-line | —                   | —                       | Yes (Rust static) | signet + cosign |
| rosary   | —                   | —                       | Yes (Rust static) | signet + cosign |

#### Homebrew Tap (`agentic-research/homebrew-tap`)

- `Formula/mache.rb` — installs two binaries: `mache` + `leyline` (kiln-built)
- `Formula/signet.rb` — signet with signet-sign WASM backend (kiln-built)
- Updated automatically by kiln's release workflow
- `brew install agentic-research/tap/mache`
- `brew install agentic-research/tap/signet`

**Batteries-included model for mache + ley-line:**

- `mache` works fully without ley-line — code intelligence, FUSE/NFS, MCP, callers/callees, refs
- `leyline` binary ships alongside but is inert until explicitly activated (`leyline daemon`)
- When running, mache auto-discovers leyline via UDS socket → enables semantic search + embeddings
- No auto-start, no background process, no surprise closed-source activity
- `brew info mache` documents leyline as an optional closed-source companion
- Users who want mache-only: `go install` from source (fully open, no LL)

Rosary and ley-line are server-side / developer tools — no standalone brew formula needed.
Ley-line ships as a companion binary inside the mache formula, not as its own formula.

#### OCI Images

- **Go tools** (mache, signet): melange APK → apko distroless image
- **Rust tools** (rosary, ley-line): krust (cargo-zigbuild → static musl → distroless OCI)
- All images: multi-arch (amd64 + arm64), signed with cosign

#### Version Reporting

`mache --version`:

```
mache v0.8.0 (go1.25, darwin/arm64)
```

`mache version` or `mache --version --verbose`:

```
mache v0.8.0
  ley-line: v0.2.0 (enabled, socket)
  build:   kiln v1.2.3
  commit:  abc1234

# Kiln injects ley-line version via ldflags at link time:
# -X cmd.LeylineVersion=${LEYLINE_TAG}
# extracted from the downloaded ley-line artifact filename or a VERSION file
  date:    2026-03-24T12:00:00Z
  signed:  signet ed25519 (sha256:deadbeef...)
```

### 4. Terraform GitHub Module (`rig/tofu/modules/github/`)

#### Provider

```hcl
terraform {
  required_providers {
    github = {
      source  = "integrations/github"
      version = "~> 6.0"
    }
  }
}

provider "github" {
  owner = "agentic-research"
  token = var.github_token
}
```

#### Repository Configuration

```hcl
variable "repositories" {
  type = map(object({
    visibility             = string           # "public" or "private"
    description            = string
    language               = string           # "go" or "rust"
    has_issues             = optional(bool, true)
    delete_branch_on_merge = optional(bool, true)
    allow_squash_merge     = optional(bool, true)
    allow_merge_commit     = optional(bool, false)
    allow_rebase_merge     = optional(bool, false)

    # CI configuration
    signet_resign          = optional(bool, false)
    dependabot             = optional(bool, true)
    copilot_review         = optional(bool, true)

    # Secrets (sensitive, from tfvars)
    secrets                = optional(map(string), {})

    # Variables
    variables              = optional(map(string), {})

    # Branch protection
    branch_protection = optional(object({
      required_checks = optional(list(string), ["ci", "pre-commit"])
      require_review  = optional(bool, true)
      enforce_admins  = optional(bool, false)
    }), {})
  }))
}
```

#### Resources Per Repo

```hcl
resource "github_repository" "repo" {
  for_each = var.repositories

  name                   = each.key
  visibility             = each.value.visibility
  description            = each.value.description
  has_issues             = each.value.has_issues
  delete_branch_on_merge = each.value.delete_branch_on_merge
  allow_squash_merge     = each.value.allow_squash_merge
  allow_merge_commit     = each.value.allow_merge_commit
  allow_rebase_merge     = each.value.allow_rebase_merge
}

resource "github_branch_protection" "main" {
  for_each = var.repositories

  repository_id = github_repository.repo[each.key].node_id
  pattern       = "main"

  required_status_checks {
    strict   = true
    contexts = each.value.branch_protection.required_checks
  }

  required_pull_request_reviews {
    required_approving_review_count = each.value.branch_protection.require_review ? 1 : 0
  }

  enforce_admins = each.value.branch_protection.enforce_admins
}

resource "github_actions_secret" "secrets" {
  for_each = merge([
    for repo_name, repo in var.repositories : {
      for secret_name, secret_value in repo.secrets :
      "${repo_name}/${secret_name}" => {
        repository  = repo_name
        secret_name = secret_name
        value       = secret_value
      }
    }
  ]...)

  repository      = each.value.repository
  secret_name     = each.value.secret_name
  encrypted_value = each.value.value  # pre-encrypted with repo public key
}

# Note: SIGNET_MASTER_KEY is the root of trust. Do NOT use plaintext_value —
# that stores the secret in TF state. Instead, pre-encrypt with the repo's
# public key (gh api /repos/{owner}/{repo}/actions/secrets/public-key)
# or use a secrets manager reference. The tfvars should contain the
# encrypted form, not the raw key material.
#
# Bootstrap: `signet authority setup-resign` can set the secret directly
# via `gh secret set` for initial setup, then TF imports it.

resource "github_actions_variable" "variables" {
  for_each = merge([
    for repo_name, repo in var.repositories : {
      for var_name, var_value in repo.variables :
      "${repo_name}/${var_name}" => {
        repository = repo_name
        var_name   = var_name
        value      = var_value
      }
    }
  ]...)

  repository    = each.value.repository
  variable_name = each.value.var_name
  value         = each.value.value
}

resource "github_repository_dependabot_security_updates" "dependabot" {
  for_each = {
    for name, repo in var.repositories : name => repo if repo.dependabot
  }

  repository = github_repository.repo[each.key].name
  enabled    = true
}
```

#### Repo Definitions (`repos.tf`)

```hcl
module "github" {
  source = "./modules/github"

  repositories = {
    mache = {
      visibility  = "public"
      language    = "go"
      description = "Structural code intelligence — FUSE, NFS, and MCP"
      signet_resign = true
      secrets = {
        SIGNET_MASTER_KEY = var.signet_master_key
        LEYLINE_PAT       = var.leyline_pat
      }
      variables = {
        SIGNET_RESIGN_ENABLED = "true"
      }
      branch_protection = {
        required_checks = ["ci", "pre-commit"]
      }
    }

    "ley-line" = {
      visibility  = "private"
      language    = "rust"
      description = "Content-addressed storage and vector embeddings"
      signet_resign = true
      secrets = {
        SIGNET_MASTER_KEY = var.signet_master_key
      }
      variables = {
        SIGNET_RESIGN_ENABLED = "true"
      }
    }

    signet = {
      visibility  = "public"
      language    = "go"
      description = "Ephemeral certificate identity and signing"
      signet_resign = true
      secrets = {
        SIGNET_MASTER_KEY = var.signet_master_key
      }
      variables = {
        SIGNET_RESIGN_ENABLED = "true"
      }
    }

    rosary = {
      visibility  = "public"
      language    = "rust"
      description = "Autonomous work orchestrator"
      signet_resign = true
      secrets = {
        SIGNET_MASTER_KEY = var.signet_master_key
        LEYLINE_PAT       = var.leyline_pat
      }
      variables = {
        SIGNET_RESIGN_ENABLED = "true"
      }
    }

    kiln = {
      visibility  = "public"
      language    = "go"
      description = "ART distribution hub — build, link, package, publish"
      signet_resign = true
      secrets = {
        SIGNET_MASTER_KEY = var.signet_master_key
        LEYLINE_PAT       = var.leyline_pat
      }
      variables = {
        SIGNET_RESIGN_ENABLED = "true"
      }
    }
  }
}
```

### 5. Signet Trust Chain

#### Signing Architecture

```
ML-DSA-44 master key (PQ root, stored in GH secret + OS keyring)
  │
  ├── Issues Ed25519 bridge certs (ephemeral, 5-min, OIDC-bound)
  │     └── Signs git commits (GitHub-compatible Ed25519)
  │
  └── Signs artifacts directly (Ed25519 CMS today, PQ authority future)
        ├── checksums-sha256.txt.sig
        ├── individual binary .sig files
        └── cosign keyless (OCI images, sigstore transparency log)
```

#### Current State (go-cms, local-only)

- `signet sign <file>` uses go-cms (in-house, unaudited, OpenSSL-interop tested)
- Ed25519 signatures via Go stdlib `crypto/ed25519`
- CMS/PKCS#7 envelope construction is custom ASN.1
- Works in CI today — pure Go binary, zero runtime deps

#### Cloud Path (signet-sign, Rust/WASM)

- `signet-sign` Rust crate uses audited RustCrypto `cms` crate
- Compiled to WASM for CF Workers (auth.rosary.bot)
- go-cms is NOT used in the cloud — only signet-sign
- Kiln-built signet bundles signet-sign WASM for audited crypto locally

#### PQ Authority (Future Milestone)

**Goal**: ML-DSA-44 root authority issues Ed25519 bridge certs.

```
ML-DSA-44 master key (quantum-safe)
  │
  └── X.509 cert issuance (ML-DSA signature on Ed25519 leaf cert)
        │
        └── Ed25519 bridge cert signs commits (GitHub-compatible)
```

**Why**: GitHub requires Ed25519 for commit verification. A PQ root means:

- Quantum computer can't forge new bridge certs (CA signature is ML-DSA)
- Ephemeral 5-minute bridge certs limit exposure window
- OIDC binding ties cert to specific GHA run

**Blockers**:

- Go `crypto/x509.CreateCertificate` does not support ML-DSA as issuer algorithm
- Must use signet-sign (Rust) for cert issuance — RustCrypto `x509-cert` + `ml-dsa`
- Authority server needs Rust/WASM backend for cert issuance path (same as cloud)

**Implementation path**:

1. signet-sign crate adds `issue_bridge_cert(master_key_mldsa, leaf_pubkey_ed25519)` FFI
1. Signet authority calls signet-sign via WASM for cert issuance when master key is ML-DSA
1. Bridge cert carries Ed25519 subject key, ML-DSA issuer signature
1. `signet-git` signs commits with Ed25519 bridge cert (unchanged)
1. Verifiers check Ed25519 commit signature + ML-DSA cert chain

**Result**: Full post-quantum provenance for commit signing without breaking GitHub compatibility.

### 6. Copilot PR Review Gating

Managed via the shared `copilot-review.yml` workflow.

```yaml
# Gating logic
- name: Guard — only runs on pull_request events
  if: ${{ github.event_name != 'pull_request' }}
  run: |
    echo "::notice::Skipping Copilot review — not a pull_request event"
    exit 0

- name: Check author association
  id: check
  run: |
    ASSOC="${{ github.event.pull_request.author_association }}"
    if [[ "$ASSOC" == "OWNER" || "$ASSOC" == "MEMBER" || "$ASSOC" == "COLLABORATOR" ]]; then
      echo "eligible=true" >> "$GITHUB_OUTPUT"
    else
      echo "eligible=false" >> "$GITHUB_OUTPUT"
    fi

- name: Request Copilot review
  if: steps.check.outputs.eligible == 'true'
  uses: actions/github-script@v7
  with:
    script: |
      await github.rest.pulls.requestReviewers({
        owner: context.repo.owner,
        repo: context.repo.repo,
        pull_number: context.issue.number,
        reviewers: ['copilot']
      });
```

TF ensures all repos call this workflow via required status checks.

## Phasing

### Phase 1: Foundation

**Goal**: Shared CI + TF repo management + basic release pipeline.

1. Create reusable workflows in `agentic-research/.github`:
   - `go-ci.yml`, `rust-ci.yml`, `pre-commit.yml`, `copilot-review.yml`
1. Create `rig/tofu/modules/github/` with repo definitions
1. `tofu apply` — wires branch protection, secrets, variables, dependabot across all repos
1. Migrate mache + signet CI to shared `go-ci.yml`
1. Migrate ley-line + rosary CI to shared `rust-ci.yml`
1. Validate pre-commit works cross-language

### Phase 2: Release Pipeline

**Goal**: Tag-driven releases with artifact signing.

**Bootstrap order** (signet must ship first so other releases can use it for signing):

1. Create `go-release.yml` and `rust-release.yml` in `.github`
1. **Bootstrap signet**: tag signet v0.3.0 using shared `go-release.yml` WITHOUT signing
   (signet can't sign its own first release — chicken-and-egg)
1. Wire `signet sign` into release workflows (Ed25519 CMS artifact signatures)
   — now that signet has a GH Release, other workflows can download the binary
1. Re-tag signet v0.3.1 WITH self-signing (signet signs its own release using v0.3.0)
1. Migrate mache, ley-line, rosary release workflows to shared versions with signing
1. Update kiln to receive dispatches from all four repos
1. Add `Formula/signet.rb` to homebrew-tap
1. Add krust builds for rosary + ley-line OCI images

### Phase 3: Trust Chain

**Goal**: Full signing from commit to distribution.

1. Activate `signet-resign` across all repos (TF sets `SIGNET_RESIGN_ENABLED=true`)
1. Create shared `signet-resign.yml` reusable workflow
1. Wire cosign + signet KMS for OCI image signing in kiln
1. Kiln-built signet bundles signet-sign WASM (audited crypto replaces go-cms)
1. Kiln-built mache bundles ley-line (already works)

### Phase 4: PQ Authority

**Goal**: Post-quantum root of trust.

1. Add `issue_bridge_cert` FFI to signet-sign Rust crate
1. ML-DSA-44 master key support in signet authority (via WASM backend)
1. PQ root issues Ed25519 bridge certs for commit signing
1. Rotate all repos to PQ master keys (TF updates `SIGNET_MASTER_KEY` secrets)
1. Document verification chain for external consumers

## Testing Strategy

- Shared workflows are tested via a **canary repo** (`agentic-research/ci-canary`) — a minimal repo that calls all shared workflows and validates they pass
- TF module uses `tofu plan` in CI (no apply) to validate config changes
- Kiln's existing smoke test (`task test`) validates the linked binary works
- Signet's existing integration tests (`test_integration.sh`) validate signing end-to-end

## Dependencies

| Dependency                                                                        | Used By                 | Notes                                   |
| --------------------------------------------------------------------------------- | ----------------------- | --------------------------------------- |
| [krust](https://github.com/imjasonh/krust)                                        | kiln (Rust OCI)         | ko for Rust, cargo-zigbuild static musl |
| [apko](https://github.com/chainguard-dev/apko)                                    | kiln (Go OCI)           | Distroless image assembly               |
| [melange](https://github.com/chainguard-dev/melange)                              | kiln (APK packaging)    | Already in use                          |
| [cosign](https://github.com/sigstore/cosign)                                      | kiln (OCI signing)      | Already in use, signet as optional KMS  |
| [GitHub TF provider](https://registry.terraform.io/providers/integrations/github) | rig                     | `integrations/github ~> 6.0`            |
| signet-sign                                                                       | signet (audited crypto) | RustCrypto cms crate, WASM target       |
| go-cms                                                                            | signet (local signing)  | In-house, unaudited, Ed25519 only       |

## Open Questions

1. **Homebrew service**: Should `brew services start mache` work for the HTTP MCP server? Kiln already has entrypoint logic for this.
1. **Canary repo**: Create `agentic-research/ci-canary` or use `.github` itself as the canary?
1. **TF state**: Add GitHub module state to existing R2 backend in rig, or separate state file?
1. **Dependabot config file**: Push `dependabot.yml` via TF (`github_repository_file`) or keep it manual?
1. **Pre-commit remote config**: Does the `ci.config` remote reference work reliably, or should each repo vendor a thin config that imports from `.github`?
