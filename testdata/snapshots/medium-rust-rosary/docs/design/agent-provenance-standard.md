# Agent Provenance Attestation Standard (APAS)

**Version**: 0.1.0-draft
**Status**: Draft
**Authors**: James Gardner, Claude (synthesis of 4-agent research team)
**Date**: 2026-03-25

> **Reading guide**: Sections marked **[CURRENT]** describe behavior that exists today
> in the rosary reference implementation. Sections marked **[TARGET]** describe the
> intended design that is not yet implemented.

## Abstract

The Agent Provenance Attestation Standard (APAS) defines a protocol for cryptographically verifiable provenance chains across autonomous AI agent pipelines. It specifies how agent orchestrators record, sign, and verify the complete chain from work decomposition through agent execution to code delivery.

APAS is implementation-agnostic. Rosary + signet serve as the reference implementation.

### Normative References

APAS builds on and references these existing specifications rather than reinventing them:

| Spec | Source | APAS Usage |
|------|--------|-----------|
| Signet Token Format | `signet/docs/design/001-signet-tokens.md` | Identity tokens (CBOR + COSE/Ed25519) |
| Signet Bridge Certificates | `signet/docs/design/004-bridge-certs.md` | Delegated identity for agents |
| Signet Identity Model | `signet/pkg/sigid/` | 4-entity decomposition (Owner/Machine/Actor/Identity) |
| Ley-line CMS Signing | `ley-line/rs/crates/sign/src/cms.rs` | Ed25519 CMS/PKCS#7 (RFC 5652 + RFC 8419) |
| in-toto Statement | https://in-toto.io/Statement/v1 | Attestation envelope format |
| DSSE | Dead Simple Signing Envelope | Signature wrapper |
| SLSA v1.0 | https://slsa.dev/spec/v1.0 | Conformance level model |

## 1. Problem Statement

AI coding agents autonomously modify source code. Current supply chain security (SBOM, SLSA, in-toto) tracks software components and build provenance but NOT agent decision chains. This gap means:

- Agent work is indistinguishable from human work after commit
- Supply chain attacks can inject malicious code via compromised agent pipelines
- No forensic trail linking code changes to the decision chain that produced them
- No way to verify that an agent operated within its authorized scope

### 1.1 The Trivy/Aqua Precedent (March 2026)

TeamPCP compromised Trivy by exploiting mutable git tag references and long-lived service account tokens. The scanner itself was replaced with a malicious version. Key lessons:

1. **Mutable references are attack vectors** — content-addressed references are required
2. **Long-lived credentials enable persistence** — short-lived, scoped credentials limit blast radius
3. **The auditor must not be the audited** — split trust between execution and attestation

### 1.2 Why SBOMs Are Insufficient

SBOMs (CycloneDX, SPDX) answer "what components are in this software?" Agent provenance answers "who decided to make this change, why, with what tools, under what authority, and can we prove it?"

| Property | SBOM | Agent Provenance |
|----------|------|-----------------|
| Scope | Components | Decisions + Actions |
| Temporal | Point-in-time | Causal chain |
| Identity | Package origin | Agent + orchestrator + user |
| Verification | Hash matching | Signature chain |
| Trust model | Publisher attestation | Multi-party attestation |

## 2. Conformance Levels

Inspired by SLSA, APAS defines four conformance levels. Each builds on the previous.

### Level 1: Audit Trail (L1) **[CURRENT — partial]**

**Requirement**: Every agent action is recorded with structured metadata.

- Dispatch manifest captures: agent identity, provider, model, permissions, bead reference, timestamps
- Tool calls logged to stream file (`.rsry-stream.jsonl` or equivalent)
- Pipeline phase transitions recorded with handoff documents
- All records are JSON, machine-parseable

**What it proves**: "We know what happened." Forensic reconstruction is possible.

**What it does NOT prove**: Records haven't been tampered with.

> **Important**: L1 provides **forensic value** (post-incident reconstruction) but
> **limited preventive value**. The orchestrator that writes provenance records is
> the same entity being audited. At L1, provenance is self-attested — useful for
> debugging and audit, but an attacker who compromises the orchestrator can forge
> records. Do not treat L1 as a security boundary.

### Level 2: Signed Attestations (L2) **[TARGET — prerequisite shipped]**

**Requirement**: Every attestation is cryptographically signed by the entity that produced it.

- Hash chain links content hashes, not file paths — **shipped** (PR #117, `Handoff::previous_chain_hash`)
- Dispatch manifests signed by orchestrator key — **not yet implemented**
- Handoff documents signed by the phase's agent key (or orchestrator on behalf) — **not yet implemented**
- Commit signatures via signet bridge certificates (see `signet/docs/design/004-bridge-certs.md`) — **not yet implemented**
- Attestations use in-toto envelope format with APAS predicate type — **not yet implemented**
- Signing uses ley-line's CMS/Ed25519 implementation (`ley-line/rs/crates/sign/`) — **not yet implemented**

**What it proves**: "We know what happened AND who attests to it." Tamper-evident.

**What it does NOT prove**: The signing entity was operating correctly.

> **Important**: Like L1, L2 is primarily **forensic**. The signing key is held by
> the orchestrator, so a compromised orchestrator can sign false attestations.
> L2's value is tamper-evidence for EXTERNAL observers (CI systems, code reviewers,
> compliance tools) — they can verify the signature chain without trusting the
> orchestrator's runtime state. But L2 alone does not prevent a compromised
> orchestrator from producing validly-signed malicious output.

### Level 3: Isolated Execution (L3) **[TARGET — foundation in ACP]**

**Requirement**: Agent execution is isolated from the attestation authority.

- Agents run in sandboxed environments (container, VM, or OS sandbox)
- The orchestrator that writes attestations cannot modify agent workspace
- Tool calls are mediated through a permission boundary (ACP `request_permission`)
- Network access is restricted to declared endpoints
- File system access is scoped to the workspace

**What it proves**: "The agent operated within declared boundaries." The fox and henhouse are separated.

**What it does NOT prove**: The agent's inputs were not poisoned.

### Level 4: Verified Inputs (L4) **[TARGET — future]**

**Requirement**: Agent inputs are themselves attested and verified.

- CLAUDE.md / system prompts are content-hashed and included in attestation
- MCP server responses are logged and hashed
- Bead descriptions are immutable after dispatch (content_hash in BeadSpec)
- Model provider responses are logged (for forensic reconstruction, not real-time verification)
- Agent binary/version is attested (SBOM of the agent itself)

**What it proves**: "The full chain from input to output is verifiable." End-to-end provenance.

## 3. Attestation Format

APAS uses the in-toto attestation framework with a custom predicate type.

### 3.1 Envelope

```json
{
  "_type": "https://in-toto.io/Statement/v1",
  "subject": [
    {
      "name": "rosary-11214e",
      "digest": { "sha256": "<bead_content_hash>" }
    }
  ],
  "predicateType": "https://rosary.bot/provenance/dispatch/v1",
  // NOTE: rosary.bot is a namespace identifier for the predicate type, not a
  // hosted service URL. The URI does not need to resolve — DSSE/in-toto
  // verifiers treat predicateType as an opaque type tag, not a fetchable doc.
  "predicate": { ... }
}
```

### 3.2 Predicate: `dispatch/v1`

```json
{
  "dispatchDefinition": {
    "beadRef": {
      "repo": "rosary",
      "beadId": "rosary-11214e",
      "contentHash": "sha256:abc123..."
    },
    "pipeline": {
      "phases": ["scoping-agent", "dev-agent", "staging-agent"],
      "currentPhase": 1,
      "pipelineId": "uuid"
    },
    "agent": {
      "name": "dev-agent",
      "definition": "sha256:<hash of agent .md file>",
      "provider": "claude",
      "model": "claude-opus-4-6",
      "permissionProfile": "implement"
    }
  },
  "runDetails": {
    "orchestrator": {
      "name": "rosary",
      "version": "0.1.0",
      "identity": {
        "signetToken": "SIG1.<payload>.<signature>",
        "bridgeCert": "<base64 DER>"
      }
    },
    "execution": {
      "workDir": "/path/to/worktree",
      "startedAt": "2026-03-25T00:00:00Z",
      "completedAt": "2026-03-25T00:05:00Z",
      "durationMs": 300000,
      "sessionId": "uuid",
      "pid": 12345,
      "isolationLevel": "git-worktree"
    },
    "work": {
      "commits": [
        {
          "sha": "abc123",
          "message": "[rosary-11214e] fix(dolt): fast-fail connect",
          "signature": "<git signature>"
        }
      ],
      "filesChanged": ["src/dolt/mod.rs", "src/scanner.rs"],
      "linesAdded": 47,
      "linesRemoved": 12
    },
    "verification": {
      "passed": true,
      "highestTier": 2,
      "tiers": [
        {"name": "commit-check", "passed": true},
        {"name": "bead-ref-check", "passed": true},
        {"name": "diff-sanity", "passed": true}
      ]
    },
    "cost": {
      "totalUsd": 0.47,
      "inputTokens": 14000,
      "outputTokens": 1678
    },
    "outcome": {
      "success": true,
      "stopReason": "end_turn",
      "beadClosed": false
    },
    "handoffChain": {
      "phaseHash": "sha256:<hash of this phase>",
      "previousPhaseHash": "sha256:<hash of previous phase>",
      "chainRoot": "sha256:<hash of phase 0>"
    }
  }
}
```

### 3.3 Signing

The envelope is signed using DSSE (Dead Simple Signing Envelope):

```json
{
  "payloadType": "application/vnd.in-toto+json",
  "payload": "<base64(attestation)>",
  "signatures": [
    {
      "keyid": "sha256:<public key hash>",
      "sig": "<base64(ed25519 signature)>"
    }
  ]
}
```

### 3.4 Signing Key Hierarchy

Signing uses the identity model defined in signet. APAS does not define its own
key format — it delegates to signet's existing specifications.

| Level | Key Type | Lifetime | Defined In |
|-------|----------|----------|------------|
| User master key | Ed25519 | Long-lived | `signet/pkg/crypto/algorithm/ed25519.go` |
| Orchestrator bridge cert | X.509 + Ed25519 | Short-lived, per-dispatch | `signet/docs/design/004-bridge-certs.md` |
| Agent session key | Ephemeral Ed25519 | Per-session | `signet/pkg/crypto/epr/proof.go` |

The 4-entity identity model from `signet/pkg/sigid/` decomposes identity as:
- **Owner**: the human user who authorized the dispatch
- **Machine**: the orchestrator instance (Fly machine, local Mac)
- **Actor**: the agent persona (dev-agent, staging-agent)
- **Identity**: the cryptographic key binding all three

Signing implementation uses ley-line's Rust CMS crate (`ley-line/rs/crates/sign/src/cms.rs`)
which supports both RFC 5652 (signed attributes) and RFC 8419 (PureEdDSA).

### 3.5 Predicate Splitting (Future)

> **Note**: The `dispatch/v1` predicate bundles dispatch definition, execution,
> work, verification, cost, and handoff chain into a single predicate. This is
> pragmatic for v0.1 but may need splitting in future versions — SLSA deliberately
> separates `buildDefinition` from `runDetails` so different parties can attest
> to different parts. A candidate split:
>
> - `dispatch-definition/v1` — what was intended (bead, pipeline, agent)
> - `dispatch-execution/v1` — what happened (timing, work, cost)
> - `dispatch-verification/v1` — what was verified (tiers, outcome)

## 4. Hash Chain Structure

### 4.1 Element Hashes **[TARGET — only Phase level is implemented]**

Each level in the hierarchy has a content hash. Currently only the Phase level
(`Handoff::chain_hash()`) and Bead level (`BeadSpec::content_hash()`) are
implemented. Lower levels (ToolCall, FileChange) and upper levels (Thread,
Decade) are target design.

```
H(FileChange) = SHA256(path || old_content || new_content)
H(ToolCall)   = SHA256(tool_name || input_hash || output_hash || timestamp)
H(Action)     = SHA256(H(ToolCall_0) || H(ToolCall_1) || ... || H(ToolCall_n))
H(Phase)      = SHA256(agent || provider || H(Action) || H(previous_phase))
H(Bead)       = SHA256(H(Phase_0) || H(Phase_1) || ... || H(Phase_n))
H(Thread)     = SHA256(H(Bead_0) || H(Bead_1) || ... || H(Bead_m))
H(Decade)     = SHA256(H(Thread_0) || H(Thread_1) || ... || H(Thread_k))
```

### 4.2 Chain Properties

- **Tamper-evident**: Modifying any element changes its hash, which propagates upward
- **Ordered**: The chain encodes temporal ordering via sequential hashing
- **Complete**: A valid chain requires all elements; gaps are detectable
- **Rooted**: The decade hash is the root of trust for the entire work decomposition

> **SHA-256 vs git SHA-1**: APAS uses SHA-256 throughout. Git commit SHAs
> (currently SHA-1, transitioning to SHA-256) are included in `Handoff::commit_shas`
> and hashed into `chain_hash()` as opaque byte strings — binding the provenance
> chain to the actual code committed. When git repos opt into SHA-256 object
> format, the commit references will be natively compatible with APAS hashes.

### 4.3 Content-Linked Chain Hash (Shipped)

> **Resolved in PR #117** (`fix(handoff): content-linked chain hash`).
> The `Handoff` struct carries `previous_chain_hash: Option<String>` —
> the hex-encoded SHA-256 produced by `chain_hash()` on the previous phase's
> `Handoff` struct (hashing phase, agent, bead_id, summary, files, commit SHAs,
> and the prior chain link — not raw JSON bytes). `chain_hash()` includes this
> hash, not a file path. Replacing a handoff file without knowing its hash breaks the chain.
>
> PR #130 added `commit_shas: Vec<String>` to `Handoff`, binding the chain to
> the actual committed code — two handoffs with identical summaries but different
> commits produce different hashes.
>
> See `src/handoff.rs` for the implementation and chain_hash test suite.

## 5. Adversarial Model

### 5.1 Threats Addressed

| Threat | L1 | L2 | L3 | L4 |
|--------|----|----|----|----|
| Forged agent identity | - | Detected | Detected | Detected |
| Tampered attestation | - | Detected | Detected | Detected |
| Phantom bead injection | - | Detected (breaks chain) | Detected | Detected |
| Unauthorized tool use | - | - | Prevented | Prevented |
| Poisoned agent input | - | - | - | Detected |
| Compromised model provider | - | - | - | Forensic only |

### 5.2 Threats NOT Addressed (Red Team Findings)

1. **Compromised model provider**: If the LLM itself is poisoned, the agent produces correctly-signed malicious code. APAS provides forensic trail but cannot prevent this. Mitigation: adversarial review phase with different provider.

2. **Honest-but-curious agent**: An agent that signs everything correctly but exfiltrates data via tool calls. APAS L3 limits network access but cannot prevent all covert channels. Mitigation: network allowlisting + tool call audit.

3. **Time-of-check-time-of-use**: File modified between attestation and commit. Mitigation: atomic workspace snapshots (jj/git stash before signing).

4. **Self-attested provenance**: The orchestrator writes its own attestations. At L1-L2, this is the fox guarding the henhouse. L3 requires separation. L4 requires external witnesses.

## 6. Relationship to Existing Standards

| Standard | Relationship |
|----------|-------------|
| SLSA | APAS levels parallel SLSA levels. APAS dispatch predicate extends SLSA provenance. |
| in-toto | APAS uses in-toto Statement/v1 envelope format and DSSE signing. |
| CycloneDX | APAS complements CycloneDX SBOM. Agent metadata could be a CycloneDX AI/ML-BOM component. |
| SCAI | APAS verification tiers parallel SCAI attribute assertions. |
| Sigstore | APAS signing chain is compatible with Sigstore's keyless signing model (via OIDC → ephemeral cert). |

## 7. Reference Implementation

### 7.1 Rosary (Orchestrator)

- `src/handoff.rs` — Phase handoff with chain hashing (L1, partial L2)
- `src/manifest.rs` — Dispatch manifest capture (L1)
- `src/session.rs` — Session tracking (L1)
- `src/acp.rs` — ACP permission handling (L3 foundation)
- `crates/bdr/` — Work decomposition with content hashing (L1)

### 7.2 Signet (Identity)

- `pkg/crypto/epr/` — Ephemeral proof-of-possession (L2)
- `pkg/crypto/algorithm/` — Ed25519 signing (L2)
- Bridge certificates — Delegated identity (L2)
- OIDC token exchange — Federated identity (L2)

### 7.3 Ley-line (Signing + Storage)

- `rs/crates/sign/` — CMS/PKCS#7 Ed25519 signing (L2)
- Arena storage — Content-addressed immutable snapshots (L2, L4)

### 7.4 Implementation Phases

| Phase | Conformance | What | Where |
|-------|-------------|------|-------|
| 0 (done) | L1 partial | Handoff chain hashing, manifest capture | rosary |
| 1 | L1 complete | Fix chain_hash to use content hashes, capture tool calls | rosary |
| 2 | L2 | Sign handoffs + manifests with Ed25519 via ley-line-sign | rosary + ley-line |
| 3 | L2 | Agent commits signed via signet bridge certs | rosary + signet |
| 4 | L3 | Container sandbox for agent execution | rosary + rig |
| 5 | L3 | ACP permission mediation as trust boundary | rosary |
| 6 | L4 | Input hashing (CLAUDE.md, bead descriptions, MCP responses) | rosary |
| 7 | L4 | External witness (transparency log, ley-line arena) | ley-line |

## 8. The 5 Whys

**Why do we need agent provenance?**
→ Because AI agents autonomously modify source code in production repositories.

**Why is that a risk?**
→ Because we cannot distinguish agent work from human work after the commit is made.

**Why does that matter?**
→ Because supply chain attacks can inject malicious code via compromised agent pipelines (Trivy precedent).

**Why can't existing tools catch this?**
→ Because SBOMs track components (static), not decision chains (temporal + causal + identity-bound).

**Why is a decision chain different from a component list?**
→ Because it requires: (1) temporal ordering of actions, (2) causal linking between phases, (3) identity binding to specific agents/users, (4) scope verification (did the agent stay within its permissions?), and (5) input/output integrity (were the agent's inputs and outputs consistent?).

**Rock bottom**: The fundamental unit of trust in software is "who changed what, when, and why." For human developers, git blame + code review provides this. For autonomous agents, we need a cryptographically verifiable equivalent. APAS is that equivalent.

## Appendix A: Glossary

- **APAS**: Agent Provenance Attestation Standard
- **Bead**: A file-scoped work item tracked in a repo's `.beads/` directory
- **BDR**: Bead Decomposition Record — how ADRs decompose into dispatchable work
- **Bridge Certificate**: Short-lived X.509 cert delegating identity from master key to agent
- **Decade**: ADR-level grouping of threads
- **DSSE**: Dead Simple Signing Envelope (in-toto signing format)
- **Handoff**: Structured context transfer between pipeline phases
- **Thread**: Ordered group of related beads
- **Manifest**: Dispatch SBOM — complete record of a single agent execution
