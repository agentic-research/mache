# Agent Provenance and Software Bill of Materials for AI Agent Systems

**Deep Research Report** | March 25, 2026

---

## Table of Contents

1. [SBOM Standards Deep Dive](#1-sbom-standards-deep-dive)
2. [Agent Provenance State of the Art](#2-agent-provenance-state-of-the-art)
3. [The Trivy/Aqua Security Breach](#3-the-trivyaqua-security-breach)
4. [BDR as Provenance Record](#4-bdr-as-provenance-record)
5. [Signet's Role in Agent Provenance](#5-signets-role-in-agent-provenance)
6. [Synthesis: The Agent Provenance Stack](#6-synthesis-the-agent-provenance-stack)

---

## 1. SBOM Standards Deep Dive

### 1.1 CycloneDX

**Current version**: v1.7 (October 2025), published as ECMA-424.

CycloneDX is a full-stack Bill of Materials standard supporting SBOM, SaaSBOM, HBOM, AI/ML-BOM, CBOM, OBOM, MBOM, VDR, and VEX. It models supply chain components as a hierarchy of `bom > components > component` with rich metadata.

**Key provenance features**:

- **v1.6 (2024)**: Added attestation support and Cryptography Bill of Materials (CBOM).
- **v1.7 (2025)**: Added `citations` for data provenance --- SBOM authors can declare where specific BOM data originated (build system, generation tool, artifact repository, manual input). This enables verifiable chains of provenance.
- **AI/ML-BOM**: Represents models, datasets, training methodologies, AI framework configurations, and dataset provenance as first-class supply chain elements.
- **Attestation predicate type**: `https://cyclonedx.org/bom` is the official predicate type for all CycloneDX BOM varieties.

**Coming in 2026**: The CycloneDX **Transparency Exchange Language (TEL)** is a superset of CycloneDX BOM that unifies BOMs with architectural blueprints, threat/behavioral/risk modeling, **AI and agentic capabilities**, compliance attestations, and post-quantum cryptography readiness.

**Relevance to agent provenance**: CycloneDX's AI/ML-BOM already models AI components, and the upcoming TEL explicitly includes "agentic capabilities." However, CycloneDX models *what components exist* (inventory) --- it does not model *what actions were taken* (execution provenance). Agent actions (tool calls, code changes, decisions) are not components in the traditional sense. CycloneDX would be the right format for documenting which models, tools, and agent definitions were used, but not for recording the execution trace.

### 1.2 SPDX 3.0

**Current version**: 3.0.1 (ISO/IEC 5962:2021 lineage).

SPDX 3.0 introduced a modular profile system with dedicated AI and Dataset profiles.

**Key provenance features**:

- **AI Profile**: 36 new fields treating datasets, models, and their provenance as first-class supply chain elements. Captures architecture, data sources, licensing, and provenance.
- **Dataset Profile**: Separated from AI profile to clarify provenance and training methods.
- **Relationship types**: `trained_on`, `tested_on`, `generated_by` express provenance relationships between AI artifacts.
- **Build Profile**: Captures build system metadata analogous to SLSA provenance.

**Relevance to agent provenance**: SPDX's AI profile is designed for ML model provenance (training data, architecture, evaluation), not for runtime agent behavior. Like CycloneDX, it answers "what was this system built from?" not "what did this system do?"

### 1.3 SLSA (Supply-chain Levels for Software Artifacts)

**Current version**: v1.1 (build track), v1.2 (specification).

SLSA provides a framework for incrementally improving supply chain security, organized into levels (L1-L3) measuring trustworthiness and completeness of provenance.

**Provenance predicate structure** (`https://slsa.dev/provenance/v1`):

```json
{
  "buildDefinition": {
    "buildType": "<TypeURI>",           // parameterized build template
    "externalParameters": {},           // untrusted inputs (must be verified)
    "internalParameters": {},           // trusted platform-set values
    "resolvedDependencies": []          // fetched artifacts (ResourceDescriptors)
  },
  "runDetails": {
    "builder": {
      "id": "<URI>",                    // builder identity
      "builderDependencies": [],        // builder's own deps
      "version": {}                     // builder version info
    },
    "metadata": {
      "invocationId": "<string>",       // unique build ID
      "startedOn": "<timestamp>",
      "finishedOn": "<timestamp>"
    },
    "byproducts": []                    // additional outputs (ResourceDescriptors)
  }
}
```

**ResourceDescriptor** fields: `uri`, `digest` (sha256/sha512/gitCommit), `name`, `downloadLocation`, `mediaType`, `content` (base64), `annotations`.

**SLSA levels**:
- **L1**: Provenance exists (package-generation attestation).
- **L2**: Hosted build platform generates provenance (signed).
- **L3**: Hardened build platform, isolated builds, unforgeable provenance.

**Relevance to agent provenance**: SLSA's model maps remarkably well to agent dispatch. The `buildDefinition` is analogous to a dispatch definition (agent, bead, parameters), and `runDetails` maps to execution metadata (provider, timestamps, session). The key insight: **agent dispatch IS a build process** --- it takes inputs (bead specification, file scopes, agent definition) and produces outputs (commits, file changes, handoff records). SLSA provenance could be adapted with a custom `buildType` for agent executions.

### 1.4 in-toto

**Current version**: ITE-6 attestation framework, actively maintained by CNCF.

in-toto is a framework for securing the integrity of software supply chains by defining **layouts** (expected supply chain steps) and collecting **link metadata** (evidence of actual execution).

**Core concepts**:

- **Layout**: Signed blueprint defining the sequence of steps and authorized functionaries. Specifies which parties can perform which steps.
- **Link**: Attestation generated during step execution, capturing materials (inputs), products (outputs), command, and byproducts.
- **Verification**: Layout policies are checked against the collected set of link attestations.

**Predicate system**: in-toto uses a typed predicate model:

```json
{
  "predicateType": "<TypeURI>",
  "predicate": { /* type-specific schema */ }
}
```

**Existing predicate types**: SLSA Provenance, in-toto Link, SPDX, CycloneDX, SCAI (Software Supply Chain Attribute Integrity), Test Result, VSA (Verification Summary Attestation).

**Custom predicates**: Organizations can define new predicate types. This is the mechanism for extending in-toto to agent provenance --- define an `AgentDispatch` predicate type with agent-specific fields.

**SCAI (Software Supply Chain Attribute Integrity)**: Particularly relevant. SCAI captures functional attributes of software artifacts and their supply chain, including conditions under which attributes arise and authenticated evidence for asserted attributes. SCAI Attribute Assertions can describe:
- Conditions of execution (what environment the agent ran in)
- Evidence for claimed attributes (verification results, test outcomes)
- Dependency graph integrity (verifiable dependency chains)

**Relevance to agent provenance**: in-toto is the natural envelope format. A rosary dispatch pipeline maps directly to an in-toto layout:
- **Layout**: "Bead X must be processed by scoping-agent, then dev-agent, then staging-agent"
- **Links**: Each phase generates a link attestation with materials (input files), products (changed files), and the command (agent invocation)
- **Verification**: The reconciler verifies that the correct sequence of agents processed the bead

### 1.5 Standards Comparison Matrix

| Standard | Models | Format | Signs | Agent Fit |
|----------|--------|--------|-------|-----------|
| CycloneDX | Components, deps, AI models | JSON/XML/Protobuf | Optional via in-toto envelope | Inventory (what was used) |
| SPDX | Packages, files, AI datasets | JSON/RDF/tag-value | Optional | Inventory (what was used) |
| SLSA | Build provenance | in-toto attestation | Required (L2+) | Execution (how it was built) |
| in-toto | Supply chain steps | JSON envelope + signature | Required | Execution (what happened) |
| SCAI | Functional attributes | in-toto predicate | Via in-toto | Properties (what was verified) |

**Conclusion**: No single standard covers agent provenance. The correct approach is **layered**:
1. **in-toto envelope** for the attestation format and signature
2. **Custom predicate** (based on SLSA Provenance schema) for agent dispatch metadata
3. **CycloneDX AI/ML-BOM** for inventorying the agent components
4. **SCAI** for verification attributes (test results, review verdicts)

---

## 2. Agent Provenance State of the Art

### 2.1 Academic Research

**"Audit Trails for Accountability in Large Language Models"** (arXiv 2601.20727, January 2026)

This paper proposes the definitive framework for LLM audit trails:

1. **Lifecycle Framework**: Specifies event types to log, required metadata per event, and governance rationales across LLM lifecycle stages (data ingestion, training, evaluation, deployment, monitoring).
2. **Reference Architecture**: System for capturing, storing, and utilizing audit logs with tamper evidence, privacy, and cross-organizational linkage.
3. **Open-Source Implementation**: Python library demonstrating the audit layer within common LLM workflows.

The audit trail is defined as: *"a chronological, tamper-evident, context-rich ledger of lifecycle events and decisions that links technical provenance (models, data, training and evaluation runs, deployments, monitoring) with governance records (approvals, waivers, and attestations)."*

**Key gap**: This paper focuses on model lifecycle (training, evaluation, deployment), not on agentic runtime behavior (tool calls, code changes, multi-step reasoning). Agent provenance is a superset of model provenance.

**"TAIBOM: Bringing Trustworthiness to AI-Enabled Systems"** (arXiv 2510.02169, October 2025)

TAIBOM extends SBOM principles to AI with three contributions missing from CycloneDX and SPDX:

1. **Structured dependency model** tailored for AI components (datasets, model weights, training code, inference config).
2. **Cryptographic integrity propagation**: On creation, each artifact is hashed and digitally signed with full provenance (source URI, timestamp, license). Derived artifacts must embed signed hashes of all ancestors.
3. **Trust attestation process**: Chain-of-trust from training data through model weights to deployed inference.

**Key insight**: TAIBOM demonstrates that CycloneDX and SPDX AI extensions "do not establish cryptographic links between AI components and cannot verify whether components have remained unchanged throughout the development lifecycle." TAIBOM enforces signed attestations across the full chain.

**"Trusted AI Agents in the Cloud"** (arXiv 2512.05951, December 2025)

Proposes that all agent interactions must be "strictly supervised and attributable: every model query, tool invocation, and network request must be mediated by enforceable policies and accompanied by provenance evidence enabling accountability and auditability." Recommends a minimum evidence bundle per workload: SBOM, signed data manifests, gateway policy snapshots, router rules, schemas, tool scopes, human-in-the-loop criteria, evaluation results, and exports to SIEM/GRC.

### 2.2 Industry Frameworks

**Microsoft Agent Governance Toolkit** (March 2026, open source)

Covers all 10 OWASP Agentic Top 10 risks. Key provenance-related components:

- **AgentMesh (Trust Layer)**: Ed25519 identity, SPIFFE/SVID credentials, trust scoring (0-1000 scale) for inter-agent communication.
- **Append-only audit log**: Immutable provenance records of all agent actions.
- **Self-integrity verification**: At startup, hashes module source files and critical enforcement function bytecodes against a published manifest. Detects supply chain attacks on the toolkit itself.
- **Certification CLI**: Produces a signed attestation on every deployment.

**ASI coverage relevant to provenance**:
- ASI-03 (Identity Abuse): Zero-trust credentials per agent
- ASI-04 (Code Execution): Runtime privilege rings + sandboxing
- ASI-09 (Trust Deficit): Full audit trails + flight recorder

**OWASP Agentic Top 10 for 2026**

The OWASP Agentic Top 10 is the definitive risk taxonomy. Provenance-relevant entries:

- **ASI-03 (Agentic Identity and Credential Misuse)**: Agents need their own identities with task-scoped, time-bound permissions and clear auditability. Not human sessions or inherited admin access.
- **ASI-04 (Agentic Supply Chain Vulnerabilities)**: Provenance and inventory for agent components. Ability to contain and kill compromise.
- **ASI-09 (Human-Agent Trust Exploitation)**: Agents can fabricate rationales or socially engineer users. Provenance of agent reasoning is needed to detect this.
- **Audit Requirements**: Comprehensive logging including tool use patterns, deviations from baseline, tamper-evident logs, and lineage metadata for forensic analysis.

### 2.3 What Exists vs. What's Missing

**What exists**:
- SBOM standards for AI model inventory (CycloneDX AI/ML-BOM, SPDX AI Profile)
- Build provenance standards (SLSA, in-toto)
- Academic frameworks for LLM audit trails
- Microsoft's agent governance toolkit with attestation
- OWASP risk taxonomy for agentic AI

**What's missing** --- and what rosary + signet could pioneer:

1. **Agent Execution Attestation Standard**: No standard predicate type for "agent X executed tool Y on artifact Z with result W." SLSA's `buildDefinition`/`runDetails` is the closest, but needs agent-specific extensions.

2. **Multi-Phase Pipeline Attestation**: No standard captures a multi-agent review pipeline (dev -> staging -> prod) as a verifiable chain. in-toto layouts come closest but need adaptation for non-deterministic agent execution.

3. **Cryptographically Signed Handoff Chain**: Rosary's handoff `chain_hash()` is SHA-256 tamper-evident but not cryptographically signed. No standard defines how agent-to-agent handoffs should be attested.

4. **Agent Identity Standard**: OWASP ASI-03 requires agent identity, but no standard defines how. The Microsoft toolkit uses Ed25519 + SPIFFE/SVID. Signet's bridge certificate model is more complete.

5. **Tool Call Attestation**: No standard captures individual tool invocations (MCP tool calls, file edits, shell commands) as attested actions within an agent session.

---

## 3. The Trivy/Aqua Security Breach

### 3.1 Timeline

| Date | Event |
|------|-------|
| Late Feb 2026 | TeamPCP exploits misconfiguration in Trivy's GitHub Actions environment, extracts a privileged access token |
| Mar 1, 2026 | Trivy team discloses incident, rotates credentials --- **but rotation was not fully comprehensive** |
| Mar 19, ~17:43 UTC | Attacker force-pushes 76 of 77 version tags in `aquasecurity/trivy-action` and all 7 tags in `aquasecurity/setup-trivy` to malicious commits. Compromised `aqua-bot` service account publishes malicious Trivy binary v0.69.4 |
| Mar 19, ~20:38 UTC | Trivy team identifies and contains the attack |
| Mar 20, 2026 | Safe versions, user guidance, and IOCs published |
| Post-Mar 20 | Aqua Security's internal GitHub organization defaced --- all 44 repositories renamed/altered using stolen service account token |

### 3.2 Attack Mechanics

**Initial access**: Misconfigured GitHub Actions environment leaked a privileged access token. This gave the attacker control over repository automation and release processes.

**Persistence**: When Trivy rotated credentials on March 1, the rotation was "not fully atomic" --- residual valid credentials remained, giving the attacker continued access.

**Tag poisoning**: The attacker force-pushed 76 version tags (of 77) to point at malicious commits. This is a **git reference attack** --- the tag names stayed the same, but the commits they pointed to changed. Any CI/CD pipeline pinning to a tag (e.g., `uses: aquasecurity/trivy-action@v3`) would silently pull the malicious version.

**Payload**: The "TeamPCP Cloud Stealer":
1. Runs silently before the real scanner (workflows appear to complete normally)
2. Dumps `Runner.Worker` process memory
3. Harvests SSH, cloud, and Kubernetes secrets
4. Encrypts data using AES-256 + RSA-4096
5. Exfiltrates to remote server
6. Fallback: creates a public repository named `tpcp-docs` in the victim's GitHub account and uploads secrets there

**Scale**: Every CI/CD pipeline using `trivy-action` with an affected tag reference was compromised. Docker Hub images were also poisoned in a secondary wave.

### 3.3 How Cryptographic Signing Would Have Prevented It

The Trivy attack exploited **mutable references** (git tags) that lacked cryptographic binding to content. Here is how each layer of a provenance system would have caught or prevented the attack:

**1. Signed Tags (basic git signing)**:
If tags were signed with a known key, force-pushing a tag would invalidate the signature. Verification tools would reject the new tag because the signature would not match. This is the simplest mitigation but requires consumers to actually verify signatures.

**2. SLSA L2+ Provenance**:
At SLSA L2, a hosted build platform generates signed provenance attestations. The provenance includes the exact source commit digest, builder identity, and build parameters. Even if tags are repointed, the provenance attestation would reference the original commit SHA. Consumers verifying provenance would detect the mismatch.

**3. in-toto Layout Verification**:
An in-toto layout could specify that releases must be produced by authorized functionaries (not `aqua-bot` with a stolen token). The layout would require specific steps (code review, test execution, signing) by specific parties. A stolen service account producing releases outside the defined layout would fail verification.

**4. Content-Addressed References (ley-line model)**:
If artifact references were content-addressed (SHA-256 of the actual binary), repointing a tag would be irrelevant. The content hash of the malicious binary would not match the expected hash. This is the approach rosary's `content_hash()` on `BeadSpec` already takes.

**5. Bridge Certificates with Time-Bounded Delegation (signet model)**:
If the `aqua-bot` service account used short-lived bridge certificates (5-minute default in signet) instead of a long-lived token, the stolen credential would have expired before the March 19 attack, even though it was extracted in late February. The credential rotation failure would have been irrelevant because the credential was already time-bounded.

**Fundamental lesson**: The Trivy attack succeeded because of three failures: (1) mutable references without integrity verification, (2) long-lived credentials without time bounds, and (3) incomplete credential rotation. All three are addressed by a proper provenance + identity system.

---

## 4. BDR as Provenance Record

### 4.1 What Rosary Already Tracks

Rosary's BDR (Bead Decomposition Record) hierarchy and dispatch pipeline already constitute a rich provenance record. Here is the current data model mapped to provenance concepts:

**BDR Hierarchy (static provenance --- what was planned)**:

| Rosary Concept | Provenance Analog | Data |
|----------------|-------------------|------|
| `Decade` | Project/initiative scope | `id`, `title`, `source_path`, `status`, `meta` (frontmatter with `depends_on`, `relates_to`) |
| `Thread` | Work stream within a project | `id`, `name`, `decade_id`, `cross_repo_refs` |
| `BeadSpec` | Work item specification | `title`, `description`, `issue_type`, `priority`, `channel`, `target_repo`, `depends_on`, `success_criteria` |
| `BeadSpec.content_hash()` | Content-addressed identity | SHA-256 of immutable definition (title, description, type, priority, criteria) |

**Dispatch Pipeline (dynamic provenance --- what happened)**:

| Rosary Concept | Provenance Analog | Data |
|----------------|-------------------|------|
| `Manifest` (`.rsry-dispatch.json`) | **Execution attestation** | `identity` (dispatch_id, bead_id, agent, provider, model, pipeline_phase, permission_profile), `session` (workspace, started/completed, duration, PID), `work` (commits, files_changed, lines), `quality` (verification_passed, tier results), `cost` (tokens, USD), `vcs` (branch, commits), `outcome` (success, error, retries) |
| `Handoff` (`.rsry-handoff-{phase}.json`) | **Phase transition attestation** | `phase`, `from_agent`, `to_agent`, `bead_id`, `provider`, `thread_id`, `summary`, `files_changed`, `lines_changed`, `review_hints`, `verdict`, `artifacts` |
| `Handoff.chain_hash()` | **Tamper-evident hash chain** | SHA-256 covering phase, from_agent, bead_id, summary, files_changed, previous_handoff path |
| `DispatchRecord` | **Execution log** | `id`, `bead_ref`, `agent`, `provider`, `started_at`, `completed_at`, `outcome`, `work_dir`, `session_id` |
| `PipelineState` | **Pipeline position** | `bead_ref`, `pipeline_phase`, `pipeline_agent`, `phase_status`, `retries`, `consecutive_reverts`, `highest_verify_tier`, `backoff_until` |
| `Verifier` tiers | **Quality gates** | commit, bead_ref, compile, test, lint, diff-sanity, review (7 tiers for Rust) |

**Bead Lifecycle (state provenance --- how state evolved)**:

The `BeadState` enum defines a labeled transition system with explicit valid transitions:

```
backlog -> open -> queued -> dispatched -> verifying -> {done, rejected, blocked}
                                                         rejected -> open (retry)
                                                         blocked -> open (unblock)
```

Each transition is recorded, and the `log_event()` method on `BeadStore` provides an audit log.

### 4.2 What Makes It Already Good

**Content addressing**: `BeadSpec.content_hash()` and `Handoff.chain_hash()` use SHA-256 to create tamper-evident records. The handoff chain specifically includes the previous handoff's reference, creating a hash chain analogous to a blockchain.

**Structured manifest**: The `.rsry-dispatch.json` manifest captures comprehensive execution metadata: who (agent + provider + model), what (bead + files + commits), when (timestamps + duration), how (pipeline_phase + permission_profile), and whether it worked (verification tiers + outcome).

**Multi-agent review pipeline**: The pipeline engine (`scoping -> dev -> staging -> prod` for features) provides inherent multi-party verification. The staging-agent adversarially reviews dev-agent's work. This is analogous to in-toto's multi-functionary layout.

**File scope isolation**: Beads have explicit file scopes, and the triage system prevents concurrent edits to the same files (`has_file_overlap`). This provides a form of blast radius control.

**Verification tiers**: The `Verifier` runs 7 tiers for Rust code (commit check, bead ref check, compile, test, lint, diff sanity, AI review). Each tier's result is recorded in the manifest's `quality` section.

### 4.3 What's Missing for Cryptographic Verifiability

Despite the rich data model, rosary's provenance has critical gaps:

**1. No cryptographic signatures**

The `chain_hash()` on `Handoff` proves integrity (tampering is detectable) but not authenticity (anyone could have written it). There is no signature binding the handoff to a specific identity. The manifest is unsigned JSON.

**Gap**: Need to sign manifests and handoffs with the dispatching entity's key. Signet bridge certificates are the natural mechanism.

**2. No attestation envelope**

Manifests and handoffs are raw JSON files, not in-toto attestation envelopes. They cannot be verified by standard supply chain verification tools (cosign, slsa-verifier, in-toto-verify).

**Gap**: Wrap manifests in in-toto Statement + DSSE (Dead Simple Signing Envelope) format. Define a custom predicate type: `https://rosary.bot/provenance/dispatch/v1`.

**3. No agent identity binding**

The manifest records `agent: "dev-agent"` and `provider: "claude"` as strings, but there is no cryptographic proof that dev-agent actually executed the work. Any process writing to the workspace could forge a manifest claiming to be dev-agent.

**Gap**: Agents need cryptographic identities. When rosary dispatches an agent, the dispatch should include a challenge (nonce) that the agent must sign with its identity key and include in the manifest.

**4. No tool call attestation**

The `.rsry-stream.jsonl` log captures agent output, but individual tool calls (MCP tool invocations, file edits, shell commands) are not individually attested. You can reconstruct what happened from the log, but cannot prove a specific tool call occurred at a specific time with specific parameters.

**Gap**: For high-assurance scenarios, each tool call should generate a micro-attestation (tool name, parameters, result hash, timestamp, agent identity).

**5. No transparency log**

Hash chains prove ordering within a pipeline, but there is no global ordering or inclusion proof. A compromised orchestrator could omit or reorder handoffs without detection.

**Gap**: Submit handoff hashes to a transparency log (Rekor or a git-based equivalent per signet's hybrid approach). This provides append-only, globally ordered evidence.

**6. Chain hash references paths, not content**

`Handoff.chain_hash()` includes `previous_handoff` as a *path string* (e.g., `.rsry-handoff-0.json`), not the actual hash of the previous handoff's content. This means the chain can be broken by renaming files or replacing content at the same path.

**Gap**: Change `previous_handoff` from a path reference to the hex-encoded `chain_hash()` of the previous handoff. This creates a true content-addressed hash chain.

---

## 5. Signet's Role in Agent Provenance

### 5.1 Signet's Capabilities

Signet provides identity and signing infrastructure with four foundational principles that align directly with agent provenance needs:

**Local-First Identity**: User's identity anchored to an Ed25519 key pair stored locally. The "Wallet" acts as its own Issuer and CA.

**DID-as-Issuer**: Every issuer (user, organization, device) is a Decentralized Identifier (DID), providing universal, interoperable identity.

**Offline-First Cryptography**: All core operations work without internet: generating proofs of possession, issuing self-attested credentials, creating short-lived certificates, verifying signatures.

**Key packages relevant to agent provenance**:

| Package | Provenance Role |
|---------|-----------------|
| `pkg/crypto/epr` | Ephemeral Proof Routines: master key signs ephemeral key, ephemeral key signs request. Two-step verification. |
| `pkg/attest/x509` | Local CA for short-lived certificates (5-minute default). Agent dispatch certificates. |
| `pkg/signet` | CBOR token structures: IssuerID, ConfirmationID, ExpiresAt, Nonce, EphemeralKeyID. |
| `pkg/git` | Git commit signing/verification via CMS/PKCS#7. Agents sign their commits. |
| `pkg/policy` | Trust policy bundles: PolicyChecker, Compiler, signed CBOR bundles with rollback protection. |
| `pkg/sigid` | Identity context extraction: 4-entity model (Owner, Machine, Actor, Identity). |
| `pkg/oidc` | OIDC token exchange: GitHub Actions OIDC -> signet bridge certificate. |
| `pkg/authflow` | Pluggable auth flow registry (venturi pattern). |
| `pkg/agent` | gRPC agent server/client for key operations. |
| `rs/crates/sign` | Ed25519 CMS/PKCS#7 in Rust (for rosary integration). |

### 5.2 The Agent Signing Chain

Signet's existing architecture maps to a three-level signing chain for agent provenance:

```
Level 0: User Master Key (Ed25519, never leaves device)
    |
    |-- signs -->
    |
Level 1: Orchestrator Bridge Certificate (short-lived, issued per dispatch session)
    |
    |-- signs -->
    |
Level 2: Agent Execution Attestation (per-phase, signed by orchestrator cert)
    |
    |-- contains -->
    |
Level 3: Agent Commit Signatures (git commits signed via signet-git)
```

**Level 0 --- User Master Key**:
The user's Ed25519 master key is the root of trust. It lives in the signet wallet on the user's device. It never leaves the device and is never exposed to agents or CI/CD systems.

**Level 1 --- Orchestrator Bridge Certificate**:
When rosary starts a dispatch session, it obtains a short-lived bridge certificate from signet's local CA (`pkg/attest/x509`). This certificate:
- Has a 5-minute default lifetime (configurable)
- Is bound to the orchestrator's DID
- Contains the dispatch context (bead_id, pipeline_phase) in certificate extensions
- Is signed by the user's master key via the EPR (Ephemeral Proof Routine) two-step process

**Level 2 --- Agent Execution Attestation**:
The orchestrator signs each dispatch manifest and handoff with the bridge certificate. This creates an in-toto-compatible attestation:
- **Subject**: The dispatch artifacts (manifest, handoffs, commits)
- **Predicate**: The dispatch metadata (agent, provider, model, files, verification results)
- **Signature**: The orchestrator's bridge certificate

**Level 3 --- Agent Commit Signatures**:
Agents running via `signet-git` sign their git commits with certificates issued by the same CA chain. The commit signature proves the agent's identity, and the dispatch attestation proves the agent was authorized to make those commits for that bead.

### 5.3 OIDC Integration for CI/CD

Signet already supports OIDC token exchange for GitHub Actions:
```
GitHub Actions OIDC Token --> signet authority exchange-github-token --> Bridge Certificate
```

This enables the same signing chain in CI/CD environments. A rosary agent dispatched via GitHub Actions can obtain a bridge certificate without long-lived secrets --- directly addressing the class of vulnerability that enabled the Trivy attack.

### 5.4 Trust Policy for Agent Dispatch

Signet's `pkg/policy` provides trust policy bundles with signed CBOR and rollback protection. These can enforce:

- **Agent authorization**: Only agents with valid certificates for the correct pipeline phase can produce attestations
- **Provider restrictions**: Limit which LLM providers an agent can use
- **File scope enforcement**: Cryptographically bind an agent's authority to specific file paths
- **Time bounds**: Bridge certificates expire, preventing stale credentials from being reused

### 5.5 The sigid 4-Entity Model

Signet's `pkg/sigid` provides a 4-entity identity model that maps directly to agent dispatch:

| Entity | Agent Provenance Meaning |
|--------|--------------------------|
| **Owner** | The user who authorized the dispatch (master key holder) |
| **Machine** | The compute environment where the agent ran |
| **Actor** | The agent identity (dev-agent, staging-agent, etc.) |
| **Identity** | The cryptographic identity (DID + bridge certificate) |

This decomposition is critical: it separates "who authorized" (Owner) from "what ran" (Actor) from "where it ran" (Machine), enabling fine-grained policy and forensics.

---

## 6. Synthesis: The Agent Provenance Stack

### 6.1 Proposed Architecture

Based on this research, here is the complete agent provenance stack for rosary + signet:

```
+------------------------------------------------------------------+
|                    Transparency / Audit Layer                      |
|  - Rekor / git-based transparency log (append-only, global order) |
|  - Human-readable dashboards (Linear integration)                 |
+------------------------------------------------------------------+
|                    Attestation Layer                               |
|  - in-toto DSSE envelope (Dead Simple Signing Envelope)           |
|  - Custom predicate: rosary.bot/provenance/dispatch/v1            |
|  - SCAI attributes for verification results                       |
+------------------------------------------------------------------+
|                    Signing Layer                                   |
|  - Signet bridge certificates (short-lived, per-dispatch)         |
|  - Ed25519 via EPR (master -> ephemeral -> request)               |
|  - signet-git for agent commit signatures                         |
+------------------------------------------------------------------+
|                    Identity Layer                                  |
|  - Signet DIDs (did:git for offline-first resolution)             |
|  - 4-entity model (Owner/Machine/Actor/Identity)                  |
|  - OIDC bridge for CI/CD (GitHub Actions -> signet cert)          |
|  - Trust policy bundles (CBOR, signed, rollback-protected)        |
+------------------------------------------------------------------+
|                    Data Layer (what rosary already has)            |
|  - BDR hierarchy (Decade -> Thread -> Bead)                       |
|  - Dispatch manifest (.rsry-dispatch.json)                        |
|  - Handoff chain (.rsry-handoff-{phase}.json + chain_hash)        |
|  - Verification tiers (7-tier quality gate)                       |
|  - Pipeline state (DispatchStore + PipelineState)                 |
|  - Bead lifecycle (labeled transition system with audit log)      |
+------------------------------------------------------------------+
|                    Inventory Layer                                 |
|  - CycloneDX AI/ML-BOM for agent component inventory             |
|  - Agent definitions, model versions, tool configurations         |
|  - Provenance of agent definitions themselves                     |
+------------------------------------------------------------------+
```

### 6.2 Custom in-toto Predicate: Agent Dispatch Attestation

Proposed predicate type: `https://rosary.bot/provenance/dispatch/v1`

Based on SLSA's `buildDefinition`/`runDetails` structure, adapted for agent execution:

```json
{
  "_type": "https://in-toto.io/Statement/v1",
  "subject": [
    {
      "name": "rosary-abc/handoff-0",
      "digest": { "sha256": "<chain_hash of handoff>" }
    },
    {
      "name": "rosary-abc/manifest",
      "digest": { "sha256": "<hash of .rsry-dispatch.json>" }
    }
  ],
  "predicateType": "https://rosary.bot/provenance/dispatch/v1",
  "predicate": {
    "dispatchDefinition": {
      "dispatchType": "https://rosary.bot/dispatch/pipeline/v1",
      "beadSpec": {
        "beadId": "rosary-abc",
        "contentHash": "<BeadSpec.content_hash()>",
        "issueType": "bug",
        "fileScopes": ["src/reconcile.rs"],
        "successCriteria": [...]
      },
      "pipelineDefinition": {
        "agents": ["scoping-agent", "dev-agent", "staging-agent"],
        "currentPhase": 1,
        "currentAgent": "dev-agent"
      },
      "agentDefinition": {
        "uri": "agents/dev-agent.md",
        "digest": { "sha256": "<hash of agent definition>" }
      },
      "provider": {
        "name": "claude",
        "model": "claude-opus-4-6",
        "permissionProfile": "implement"
      }
    },
    "runDetails": {
      "orchestrator": {
        "id": "https://rosary.bot/orchestrator/v1",
        "version": "0.1.0",
        "signetDid": "did:git:rosary:abc123"
      },
      "metadata": {
        "dispatchId": "d-test-001",
        "invocationId": "<session_id>",
        "startedOn": "2026-03-25T10:00:00Z",
        "finishedOn": "2026-03-25T10:15:00Z"
      },
      "execution": {
        "workDir": "/tmp/.rsry-workspaces/rosary-abc",
        "vcsKind": "git",
        "baseBranch": "main",
        "baseCommit": "abc123",
        "headCommit": "def456"
      },
      "verification": {
        "passed": true,
        "highestPassingTier": 6,
        "tiers": [
          { "name": "commit", "result": "pass" },
          { "name": "bead_ref", "result": "pass" },
          { "name": "compile", "result": "pass" },
          { "name": "test", "result": "pass" },
          { "name": "lint", "result": "pass" },
          { "name": "diff-sanity", "result": "pass" },
          { "name": "review", "result": "pass", "verdict": "approve" }
        ]
      },
      "cost": {
        "totalCostUsd": 0.042,
        "inputTokens": 18500,
        "outputTokens": 3200
      },
      "outcome": {
        "success": true,
        "beadClosed": true,
        "filesChanged": ["src/reconcile.rs", "src/reconcile_test.rs"],
        "linesAdded": 47,
        "linesRemoved": 12
      }
    },
    "handoffChain": {
      "previousHash": null,
      "currentHash": "<chain_hash_hex>"
    }
  }
}
```

### 6.3 Implementation Roadmap

**Phase 1: Sign what exists** (Low effort, high impact)
- Sign `.rsry-dispatch.json` manifests with signet bridge certificates
- Sign handoff files with the orchestrator's bridge certificate
- Use `rs/crates/sign` (signet's Rust CMS/PKCS#7 crate) for in-process signing
- Fix `chain_hash()` to reference previous handoff's content hash, not path

**Phase 2: Wrap in in-toto envelopes** (Medium effort)
- Define the `rosary.bot/provenance/dispatch/v1` predicate type
- Wrap signed manifests/handoffs in DSSE envelopes
- Produce verification-ready attestation bundles per dispatch

**Phase 3: Agent identity** (Medium effort, requires signet changes)
- Issue per-agent bridge certificates during dispatch
- Agents sign their commits via `signet-git`
- Agent identity bound to dispatch attestation
- Policy bundles restrict agent authority to specific file scopes

**Phase 4: Transparency log** (Higher effort)
- Submit attestation hashes to Rekor or git-based transparency log
- Enable third-party verification of dispatch history
- Cross-repo attestation verification (BDR cross-repo dependencies)

**Phase 5: Component inventory** (Lower priority)
- Generate CycloneDX AI/ML-BOM for each dispatch
- Include agent definition hashes, model versions, tool configurations
- Publish alongside attestations for complete supply chain transparency

### 6.4 What This Would Have Caught

Applying this provenance stack to the Trivy scenario:

1. **Signed tags** (Phase 1): Force-pushing tags would invalidate signatures.
2. **Short-lived bridge certs** (Phase 1): The stolen `aqua-bot` token would be a 5-minute certificate, expired weeks before the March 19 attack.
3. **in-toto layout** (Phase 2): The malicious release would fail layout verification --- wrong functionary, wrong build steps.
4. **Transparency log** (Phase 4): The malicious release would appear in the log with a different builder identity, immediately visible to monitors.
5. **Content-addressed references** (Phase 1): References to artifacts by content hash rather than mutable tags would make repointing impossible.

---

## Sources

### SBOM Standards
- [CycloneDX Specification Overview](https://cyclonedx.org/specification/overview/)
- [CycloneDX AI/ML-BOM](https://cyclonedx.org/capabilities/mlbom/)
- [CycloneDX v1.7 JSON Reference](https://cyclonedx.org/docs/1.7/json/)
- [SPDX 3.0.1 AI Profile](https://spdx.github.io/spdx-spec/v3.0.1/model/AI/AI/)
- [Implementing AI BOM with SPDX 3.0 (Linux Foundation)](https://www.linuxfoundation.org/hubfs/LF%20Research/lfr_spdx_aibom_102524a.pdf)
- [SLSA Provenance v1.0](https://slsa.dev/spec/v1.0/provenance)
- [SLSA Security Levels](https://slsa.dev/spec/v1.0/levels)
- [SLSA Specification v1.2](https://slsa.dev/spec/v1.2/)
- [in-toto Attestation Framework](https://github.com/in-toto/attestation)
- [in-toto Predicate Specification](https://github.com/in-toto/attestation/blob/main/spec/v1/predicate.md)
- [in-toto Link Predicate](https://github.com/in-toto/attestation/blob/main/spec/predicates/link.md)
- [SCAI Predicate](https://github.com/in-toto/attestation/blob/main/spec/predicates/scai.md)
- [Introduction to SLSA (Chainguard Academy)](https://edu.chainguard.dev/compliance/slsa/what-is-slsa/)

### Agent Provenance Research
- [Audit Trails for Accountability in Large Language Models (arXiv 2601.20727)](https://arxiv.org/html/2601.20727v1)
- [TAIBOM: Bringing Trustworthiness to AI-Enabled Systems (arXiv 2510.02169)](https://arxiv.org/html/2510.02169)
- [Trusted AI Agents in the Cloud (arXiv 2512.05951)](https://arxiv.org/html/2512.05951v1)
- [Building an Open AIBOM Standard in the Wild (arXiv 2510.07070)](https://arxiv.org/html/2510.07070v1)
- [Microsoft Agent Governance Toolkit](https://github.com/microsoft/agent-governance-toolkit)
- [OWASP Top 10 for Agentic Applications 2026](https://genai.owasp.org/resource/owasp-top-10-for-agentic-applications-for-2026/)

### Trivy Breach
- [Aqua Security: Trivy Supply Chain Attack (official blog)](https://www.aquasec.com/blog/trivy-supply-chain-attack-what-you-need-to-know/)
- [CrowdStrike: From Scanner to Stealer](https://www.crowdstrike.com/en-us/blog/from-scanner-to-stealer-inside-the-trivy-action-supply-chain-compromise/)
- [Wiz: Trivy Compromised by TeamPCP](https://www.wiz.io/blog/trivy-compromised-teampcp-supply-chain-attack)
- [The Hacker News: Trivy GitHub Actions Breached](https://thehackernews.com/2026/03/trivy-security-scanner-github-actions.html)
- [Palo Alto Networks: Trivy Supply Chain Attack](https://www.paloaltonetworks.com/blog/cloud-security/trivy-supply-chain-attack/)
- [GitHub Advisory: GHSA-69fq-xp46-6x23](https://github.com/aquasecurity/trivy/security/advisories/GHSA-69fq-xp46-6x23)
- [GitGuardian: Secret Exposure Analysis](https://blog.gitguardian.com/trivys-march-supply-chain-attack-shows-where-secret-exposure-hurts-most/)

### Industry Standards and Compliance
- [OpenSSF: Beyond the SBOM - Ensuring Integrity with Attestations](https://openssf.org/blog/2025/03/25/beyond-the-software-bill-of-materials-sbom-ensuring-integrity-with-attestations-event-recap/)
- [ISACA: The Growing Challenge of Auditing Agentic AI](https://www.isaca.org/resources/news-and-trends/industry-news/2025/the-growing-challenge-of-auditing-agentic-ai)
- [AI Security 2026: Complete Guide (EU AI Act August 2026 deadline)](https://www.levo.ai/resources/blogs/ai-security-the-complete-guide-for)
