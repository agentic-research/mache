# Agent Provenance Lattice: Formal Specification

## Abstract

This document defines the formal mathematical structure of agent provenance in rosary. Work decomposes hierarchically: ADR, Decade, Thread, Bead, Pipeline Phase, Agent Action, Tool Call, File Change. We model this hierarchy as a bounded graded poset, define provenance as a covariant functor into signed attestations, extend the existing hash chain infrastructure to the full hierarchy, and analyze the adversarial model.

The specification grounds existing rosary infrastructure (`Handoff::chain_hash`, `BeadSpec::content_hash`, the BDR channel lattice) in a unified mathematical framework, identifies gaps, and prescribes concrete extensions.

---

## 1. The Provenance Poset

### 1.1 Elements and Grades

Let P be a finite partially ordered set (poset) of provenance elements. Each element has a **grade** (rank) in the hierarchy.

**Definition 1.1 (Grade function).** Define the grade function rho: P -> {0, 1, 2, 3, 4, 5, 6, 7} as:

| Grade | Element Type | Rosary Type | Example |
|-------|-------------|-------------|---------|
| 7 | ADR | Source markdown | `0004-dual-state-machine.md` |
| 6 | Decade | `DecadeRecord` | `ADR-004` |
| 5 | Thread | `ThreadRecord` | `ADR-004/implementation` |
| 4 | Bead | `Bead` / `BeadSpec` | `rsry-abc` |
| 3 | Pipeline Phase | `PipelineState` + `Handoff` | Phase 0 (dev-agent) |
| 2 | Agent Action | `DispatchRecord` | Single agent execution |
| 1 | Tool Call | Stream JSON event | `mcp__mache__search(...)` |
| 0 | File Change | Git diff hunk | `src/reconcile.rs:420 +3/-1` |

**Remark.** The grade ordering is *descending* by convention: higher grade = coarser granularity = more abstract. This aligns with the existing `BdrChannel::visibility_level()` where Decade=0 < Thread=1 < Bead=2, but inverts the numbering for the full 8-level hierarchy. The inversion is intentional: grade 7 is the "top" of the poset (most abstract), grade 0 is the "bottom" (most concrete).

### 1.2 Partial Order

**Definition 1.2 (Containment order).** For elements a, b in P, define a <= b if and only if a is *contained within* b in the decomposition hierarchy. Formally:

- FileChange f <= ToolCall t iff f was produced by t
- ToolCall t <= AgentAction a iff t occurred during a
- AgentAction a <= Phase p iff a is part of pipeline phase p
- Phase p <= Bead b iff p is a phase in the pipeline processing b
- Bead b <= Thread th iff b is a member of th (via `HierarchyStore::add_bead_to_thread`)
- Thread th <= Decade d iff th.decade_id = d.id
- Decade d <= ADR r iff d was decomposed from r (via `build_decade`)

This is indeed a partial order:

- **Reflexivity**: a <= a (every element contains itself).
- **Antisymmetry**: If a <= b and b <= a, then a and b are at the same grade and mutually contained, hence a = b.
- **Transitivity**: Containment composes: if a file change is within a tool call, and that tool call is within an agent action, then the file change is within the agent action.

### 1.3 Lattice Properties

**Theorem 1.1.** (P, <=) is a bounded graded poset but NOT a lattice in general.

**Proof sketch.** The poset is:
- **Bounded**: We can adjoin a formal top element T (the "project" or "system") and bottom element B (the empty provenance). Every ADR <= T and B <= every FileChange.
- **Graded**: The grade function rho satisfies: if a < b and there is no c with a < c < b, then rho(b) = rho(a) + 1. This follows from the strict layering of the hierarchy.

However, it is NOT a lattice because meets and joins do not always exist within the hierarchy:

**Counterexample (join failure).** Consider two beads b1, b2 in different threads th1, th2 of the same decade d. Their join (least upper bound) in the poset would need to be the smallest element containing both. If th1 and th2 are incomparable (neither contains the other), then the join of b1 and b2 is d (the decade). But if b1 and b2 are in different decades d1, d2 under the same ADR, the join is the ADR. The join always exists in this tree-structured case.

**Counterexample (meet failure).** Consider two file changes f1, f2 made by different tool calls in different agent actions within the same phase. Their meet (greatest lower bound) would need to be the largest element contained in both. But f1 and f2 share no common descendant. The meet is B (bottom). This always exists because we adjoined B.

**Corollary.** With the formal top T and bottom B adjoined, (P, <=) is a **bounded** poset. In the common case where the hierarchy is tree-structured (no sharing), it IS a lattice: the join is the lowest common ancestor and the meet is B for incomparable elements or the lower element for comparable ones. In practice, the hierarchy is a forest of trees (one per ADR), and adjoining T and B makes it a complete lattice.

**Definition 1.3 (Provenance lattice).** Define L = P union {T, B} with:
- B <= x for all x in P
- x <= T for all x in P
- join(a, b) = lowest common ancestor in the hierarchy tree (or T if in different ADRs)
- meet(a, b) = B if a and b are incomparable; otherwise min(a, b) if comparable

Then (L, <=, join, meet, B, T) is a bounded lattice.

**Remark on sharing.** The lattice structure breaks if a bead belongs to multiple threads, or a file change is produced by multiple tool calls. The current rosary implementation enforces single-parent containment (`find_thread_for_bead` returns `Option<String>`, not `Vec<String>`), which preserves the tree structure. This is a design constraint worth maintaining.

### 1.4 The Grading and BDR Channels

The existing `BdrChannel` enum (Decade, Thread, Bead) captures grades 6, 5, 4 of our 8-level hierarchy. The `visibility_level()` method assigns:

```
Decade.visibility_level() = 0  ->  grade 6
Thread.visibility_level() = 1  ->  grade 5
Bead.visibility_level()   = 2  ->  grade 4
```

The visibility ordering is the *reverse* of the grade ordering by convention (more visible = more concrete = lower grade). The BDR channels are the "upper lattice" concerning work decomposition; grades 0-3 are the "lower lattice" concerning execution.

**Definition 1.4 (Upper and lower lattice).** Partition L into:
- Upper lattice U = {T, ADR, Decade, Thread, Bead}: work decomposition (what)
- Lower lattice D = {Phase, Action, ToolCall, FileChange, B}: execution trace (how)
- The interface between U and D is the Bead level (grade 4)

This partition corresponds to the "dual state machine" from ADR-004: beads (U) are user-facing persistent state; pipeline phases and below (D) are infrastructure state.

---

## 2. Provenance as a Functor

### 2.1 The Category of Provenance Elements

**Definition 2.1 (Category Prov).** Define the category **Prov** whose:
- Objects are elements of L (the provenance lattice)
- Morphisms are the ordering relations: for each a <= b, there is a unique morphism iota_{a,b}: a -> b (the inclusion)
- Composition is transitivity: iota_{b,c} . iota_{a,b} = iota_{a,c}
- Identity is reflexivity: iota_{a,a} = id_a

This is the *category of a poset*, a standard construction.

### 2.2 The Category of Attestations

**Definition 2.2 (Category Att).** Define the category **Att** whose:
- Objects are *signed attestations*: tuples (content_hash, agent_id, timestamp, parent_hashes, signature)
- Morphisms are *hash inclusions*: for attestations A, B, there is a morphism A -> B if A.content_hash appears in B.parent_hashes (B attests to containing A)
- Composition: if A's hash is in B's parents and B's hash is in C's parents, then A -> C (transitivity of attestation chain)

Each attestation is:

```
Attestation {
    content_hash:   [u8; 32],       // SHA-256 of the element's content
    agent_id:       PublicKey,       // Ed25519 public key of attesting agent
    timestamp:      DateTime<Utc>,   // Wall-clock time of attestation
    parent_hashes:  Vec<[u8; 32]>,  // Content hashes of contained elements
    grade:          u8,             // Grade in the provenance lattice
    signature:      [u8; 64],       // Ed25519 signature over (content_hash || parent_hashes || grade)
}
```

### 2.3 The Provenance Functor

**Definition 2.3 (Provenance functor).** Define P: **Prov** -> **Att** as:

On objects:
- P(FileChange) = attestation with content_hash = SHA256(diff_hunk), no parents
- P(ToolCall) = attestation with content_hash = SHA256(tool_name || args || result), parents = {P(f).content_hash : f is a FileChange produced by this call}
- P(AgentAction) = attestation with parents = {P(t).content_hash : t is a ToolCall in this action}
- P(Phase) = attestation with parents = {P(a).content_hash : a is an AgentAction in this phase}. **This is the existing `Handoff::chain_hash()`, extended.**
- P(Bead) = attestation with parents = {P(p).content_hash : p is a Phase processing this bead}
- P(Thread) = attestation with parents = {P(b).content_hash : b is a Bead in this thread}
- P(Decade) = attestation with parents = {P(th).content_hash : th is a Thread in this decade}
- P(ADR) = attestation with parents = {P(d).content_hash : d is a Decade from this ADR}

On morphisms: For iota_{a,b}: a -> b, the functor maps to the hash inclusion P(a) -> P(b), which exists because P(a).content_hash is in P(b).parent_hashes by construction.

**Theorem 2.1 (Functoriality).** P preserves composition and identity.

**Proof.**
- Identity: P(id_a) maps to the identity morphism on P(a) (an attestation's hash is trivially "included" in itself).
- Composition: If a <= b <= c, then P(a).content_hash is in P(b).parent_hashes and P(b).content_hash is in P(c).parent_hashes. By the transitive closure of hash inclusion, P(a) -> P(c). This is exactly P(iota_{a,c}) = P(iota_{b,c}) . P(iota_{a,b}).

**Theorem 2.2 (Order preservation).** If a <= b in L, then P(a).content_hash is reachable from P(b) by following parent_hashes. Equivalently, P(b) "contains" the attestation for P(a).

**Proof.** Direct from the definition: each P(x) at grade k includes in its parent_hashes the content hashes of all elements at grade k-1 that it contains. By induction on grade difference, any descendant's hash is reachable.

### 2.4 Relationship to Existing Code

| Lattice level | Existing hash | Functor image P(x) |
|---------------|--------------|---------------------|
| BeadSpec (definition) | `BeadSpec::content_hash()` | Content identity, no execution parents |
| Bead (instance) | `Bead::generation()` (SipHash, non-crypto) | Needs cryptographic upgrade |
| Phase | `Handoff::chain_hash()` | Close, but path-linked not hash-linked |
| Dispatch | `Manifest` (no hash) | Needs content_hash field |
| ToolCall | Stream JSON events (no hash) | Needs per-event hashing |
| FileChange | Git commit SHA | Already cryptographic |

**Critical gap identified**: `Handoff::chain_hash()` includes `artifacts.previous_handoff` as a *file path string*, not the *hash of the previous handoff's content*. This breaks the functor property at the Phase level: replacing the previous handoff file does not invalidate the current handoff's hash. See Section 3.2 for the fix.

---

## 3. Hash Chain Properties

### 3.1 Hash Chain Definitions

**Definition 3.1 (Element hash).** For each element x in the lattice, define:

```
H(x) = SHA256(grade(x) || type(x) || content(x) || H(child_0) || H(child_1) || ... || H(child_n))
```

where children are ordered deterministically (e.g., by their own hash, or by a canonical ordering such as phase number for phases within a bead).

Concretely:

**Grade 0 (FileChange):**
```
H(file_change) = SHA256(0x00 || file_path || git_blob_hash)
```
This piggybacks on git's content-addressable storage. The git blob hash is already SHA-1 (or SHA-256 in newer git); we wrap it in our own SHA-256 for uniformity.

**Grade 1 (ToolCall):**
```
H(tool_call) = SHA256(0x01 || tool_name || "\0" || args_json || "\0" || H(fc_0) || ... || H(fc_n))
```

**Grade 2 (AgentAction):**
```
H(agent_action) = SHA256(0x02 || agent_id || "\0" || H(tc_0) || ... || H(tc_n))
```

**Grade 3 (Phase):**
```
H(phase) = SHA256(0x03 || phase_number_le32 || agent_name || "\0" || bead_id || "\0"
                   || summary || "\0" || H(action_0) || ... || H(action_n)
                   || H(previous_phase))
```

This is the corrected version of `Handoff::chain_hash()`. The critical difference: instead of including the file path of the previous handoff, we include `H(previous_phase)` -- the actual hash of the previous phase's content. For phase 0, `H(previous_phase)` is a sentinel value (32 zero bytes).

**Grade 4 (Bead):**
```
H(bead) = SHA256(0x04 || bead_id || "\0" || content_hash(bead_spec) || "\0"
                 || H(phase_0) || H(phase_1) || ... || H(phase_n))
```

Note: `content_hash(bead_spec)` is the existing `BeadSpec::content_hash()` which captures the immutable definition. The bead hash additionally includes the phase hashes, capturing the full execution history.

**Grade 5 (Thread):**
```
H(thread) = SHA256(0x05 || thread_id || "\0" || H(bead_0) || H(bead_1) || ... || H(bead_m))
```

Beads are ordered by their position in the thread (as determined by `list_beads_in_thread`).

**Grade 6 (Decade):**
```
H(decade) = SHA256(0x06 || decade_id || "\0" || H(thread_0) || ... || H(thread_k))
```

Threads are ordered alphabetically by thread_id for determinism.

**Grade 7 (ADR):**
```
H(adr) = SHA256(0x07 || adr_path || "\0" || H(decade))
```

### 3.2 Fixing the Chain Hash

The current `Handoff::chain_hash()` has a structural weakness:

```rust
// Current (path-linked):
if let Some(ref prev) = self.artifacts.previous_handoff {
    hasher.update(prev.as_bytes());  // This is ".rsry-handoff-0.json"
}
```

The fix:

```rust
// Corrected (hash-linked):
if let Some(ref prev_hash) = self.previous_chain_hash {
    hasher.update(prev_hash);  // This is H(previous_phase) = [u8; 32]
}
```

This requires adding a `previous_chain_hash: Option<[u8; 32]>` field to the `Handoff` struct, populated by the orchestrator when writing each handoff. The orchestrator computes `phase_0.chain_hash()`, then passes it to phase_1's handoff construction.

### 3.3 Properties of the Hash Chain

**Theorem 3.1 (Tamper evidence).** If any element x in the lattice is modified after its hash H(x) has been computed and recorded in its parent's hash, the modification is detectable.

**Proof.** SHA-256 is collision-resistant (under standard cryptographic assumptions). Modifying x changes H(x). Since the parent y includes H(x) in its hash computation, H(y) also changes. By induction up the lattice, H(ADR) changes. Any verifier holding the original H(ADR) can detect the tamper.

**Theorem 3.2 (Ordering).** The hash chain encodes the temporal ordering of phases within a bead.

**Proof.** Phase k's hash includes H(phase_{k-1}) as input. Reordering phases would change the hash chain: if we swap phases k and k+1, phase k+1's hash would now need to include phase k's hash (not phase k-1's), producing a different hash. The hash chain is a commitment to a specific linear order.

**Theorem 3.3 (Completeness).** Given H(bead), verifying it requires the complete set of phase hashes. No phase can be omitted.

**Proof.** H(bead) = SHA256(... || H(phase_0) || ... || H(phase_n)). To recompute H(bead), a verifier needs all n+1 phase hashes. If any phase is omitted, the recomputed hash will not match. (This assumes the verifier knows the expected number of phases, which is recorded in the pipeline metadata.)

**Theorem 3.4 (Non-repudiation, conditional on signatures).** If attestations are signed with agent private keys, no agent can deny having produced a specific action.

**Proof.** The attestation for an action includes the agent's public key and a signature over (content_hash || parent_hashes || grade). Verifying the signature proves the key holder attested to this content. Combined with Theorem 3.1, modifying the content invalidates the signature. This reduces non-repudiation to key management (see Section 4.3).

### 3.4 Incremental Verification

A key practical property: verification can be incremental. You do not need to re-hash the entire lattice to verify a single bead.

**Definition 3.2 (Merkle path).** For any element x at grade g, its Merkle path to the root is:

```
x -> parent(x) -> parent(parent(x)) -> ... -> ADR
```

The Merkle path is a sequence of (hash, sibling_hashes) pairs. To verify x, compute H(x), then for each ancestor, verify that the recorded hash matches the recomputed hash from its children including the sibling hashes.

This is O(depth * max_branching_factor) = O(8 * B) where B is the maximum number of children at any level. In practice, B is small (a bead has 2-4 phases, a thread has 5-20 beads, a decade has 3-7 threads).

---

## 4. Adversarial Model

### 4.1 Threat Actors

We consider four adversarial capabilities, ordered by increasing power:

**A1: External observer.** Can read public artifacts (PRs, commits, Linear issues) but cannot modify them. Goal: determine which code was agent-generated vs human-written. The provenance lattice makes this *easier* for honest systems (the audit trail is public) and *harder* for adversaries to fake (forging the hash chain requires the signing keys).

**A2: Compromised agent.** One agent in the pipeline has been subverted (e.g., prompt injection, model jailbreak). The agent can produce arbitrary output within its execution sandbox. Goal: inject malicious code that passes verification.

Mitigations from the lattice:
- Cross-model adversarial review (Gemini reviewing Claude's work) means a compromised Claude-based dev-agent must also compromise the Gemini-based staging-agent
- Hash chain integrity: the compromised agent's output is recorded in a signed attestation. Post-hoc audit reveals the malicious content at the exact point of injection
- File scope isolation: the compromised agent can only modify files in the bead's scope. It cannot alter `src/auth.rs` if the bead scopes to `src/reconcile.rs`

**A3: Phantom bead injection.** An attacker creates a bead in the `.beads/` Dolt database that was never decomposed from an ADR. This bead processes through the pipeline and produces code changes.

Detection via the lattice:
- Every legitimate bead has a Merkle path to an ADR: bead -> thread -> decade -> ADR
- A phantom bead has no thread or decade parent. `find_thread_for_bead()` returns `None`
- **Enforcement**: the dispatch gate should verify `find_thread_for_bead()` is `Some` for any bead entering the pipeline. Unaffiliated beads (created directly by users, not via BDR decomposition) are allowed but flagged differently in the provenance record.

**A4: Compromised signing key.** An attacker obtains an agent's private key. They can now produce valid-looking attestations for any content.

This is the most severe threat. Mitigations:
- Key rotation: signing keys should be ephemeral (per-session or per-day)
- Key escrow: the orchestrator (rosary) holds the signing keys, not the agents. Agents receive a derived session key with limited scope
- Multi-party attestation: require attestations from multiple agents (the pipeline model already provides this -- dev, staging, prod all attest)

### 4.2 Attack Trees

**Attack: Inject malicious code via agent pipeline**

```
Goal: malicious code in production
|
+-- Subvert dev-agent
|   +-- Prompt injection via bead description
|   +-- Model vulnerability (jailbreak)
|   +-- Supply chain attack on agent definition file
|   |
|   +-- AND: Bypass staging review
|       +-- Subvert staging-agent (different model, different provider)
|       +-- Make change too subtle for review (obfuscation)
|       +-- Exploit review scope gap (change affects file not in scope)
|
+-- Inject phantom bead
|   +-- Write to .beads/ Dolt database directly
|   +-- AND: Phantom bead must have file scopes matching target
|   +-- AND: Must bypass thread affiliation check
|
+-- Compromise signing key
    +-- Extract key from orchestrator process memory
    +-- Steal key from filesystem (~/.rsry/ or signet store)
    +-- AND: Forge attestation chain for all pipeline phases
```

### 4.3 What the Lattice Prevents

**Theorem 4.1 (Phantom bead detection).** Under the provenance functor P, a phantom bead (one without a Merkle path to an ADR) is detectable in O(1) time.

**Proof.** Query `find_thread_for_bead(bead_ref)`. If `None`, the bead has no thread parent and thus no path to a decade or ADR. This is a necessary (but not sufficient) condition for being a phantom. Sufficient condition: additionally verify that the thread's decade exists and was decomposed from an ADR with a valid content hash.

**Theorem 4.2 (Replay detection).** An attestation replayed from a previous execution has a different parent hash chain and is thus detectable.

**Proof.** Each attestation includes a timestamp and the parent hashes of its children. Even if the content hash of a tool call is identical across two executions (same tool, same args, same result), the parent's attestation will differ because the timestamp and agent session are different. The bead-level hash will differ because it includes all phase hashes, which include timestamps.

**Theorem 4.3 (Ordering violation detection).** If an attacker reorders phases in the pipeline (e.g., claims staging review happened before dev implementation), the hash chain is invalid.

**Proof.** Phase k's hash includes H(phase_{k-1}) as a chained input. Reordering produces a different chain. The bead-level hash H(bead) = SHA256(... || H(phase_0) || ... || H(phase_n)) will differ from the expected value.

### 4.4 Residual Risks

The lattice does NOT prevent:

1. **Compromised signing key**: If the orchestrator's signing key is compromised, valid-looking attestations can be forged. Mitigation: key rotation, HSM storage, multi-party signing.

2. **Colluding agents across models**: If both the Claude-based dev-agent and the Gemini-based staging-agent are simultaneously subverted (e.g., via a shared vulnerability in their prompting), the cross-model review provides no protection. Mitigation: diverse model providers, human review gate.

3. **Subtle malicious changes**: The lattice provides *integrity* (you can verify what happened) but not *semantic correctness* (you cannot prove the code does what it claims). An agent could introduce a subtle backdoor that passes all automated tests. Mitigation: human review of PRs, fuzzing, formal verification of critical paths.

4. **Denial of provenance**: An attacker who controls the `.beads/` database could delete or corrupt provenance records. Mitigation: the Dolt backend provides git-like history (commits, branches, merge audit trail). Deleting a bead requires a Dolt commit that is itself auditable.

5. **Time manipulation**: If an attacker controls the system clock, timestamps in attestations are unreliable. Mitigation: use monotonic counters alongside wall-clock time; cross-reference with external time sources (git commit timestamps, Linear API timestamps).

---

## 5. Verification Contravariance

### 5.1 Verification as a Contravariant Functor

Provenance flows *upward* (tool calls compose into actions compose into phases). Verification flows *downward* (a verified decade implies verified threads implies verified beads). This is a contravariant relationship.

**Definition 5.1 (Verification predicate).** For each element x in L, define V(x) in {verified, unverified, failed} as:

- V(FileChange) = verified iff git blob hash matches and commit signature is valid
- V(ToolCall) = verified iff all child file changes are verified
- V(AgentAction) = verified iff all child tool calls are verified
- V(Phase) = verified iff all child agent actions are verified AND the phase verdict is "approve"
- V(Bead) = verified iff all child phases are verified (all pipeline phases passed)
- V(Thread) = verified iff all child beads are verified
- V(Decade) = verified iff all child threads are verified
- V(ADR) = verified iff all child decades are verified

**Theorem 5.1 (Downward propagation).** If V(x) = verified, then for all y <= x, V(y) = verified.

**Proof.** By induction on grade. If x is at grade g and V(x) = verified, then by definition all children of x (at grade g-1) are verified. By the induction hypothesis, all descendants of those children are also verified. Since y <= x means y is a descendant of x, V(y) = verified.

**Theorem 5.2 (Upward propagation of failure).** If V(x) = failed for some x, then for all y >= x, V(y) != verified (it is either unverified or failed).

**Proof.** If x is at grade g and V(x) = failed, then x's parent at grade g+1 has a child that is not verified, so the parent cannot be verified. By induction, no ancestor of x can be verified.

### 5.2 Connection to Accretion

The existing `accrete()` function in `crates/bdr/src/accrete.rs` implements a specific instance of upward propagation: bead completion events flow upward to determine decade status transitions. This is precisely the verification functor applied to the upper lattice:

```
CompletionEvent {outcome: Done} -> V(bead) = verified
thread_progress(thread, completed) = |{b in thread : V(b) = verified}| / |thread.beads|
decade_progress(decade, completed) = average of thread_progress across threads
should_transition: Proposed -> Active (progress > 0) or Active -> Completed (progress = 1.0)
```

The `should_transition` function is the decision procedure for V(Decade): a decade is verified (Completed) when all its beads are verified (Done).

### 5.3 Partial Verification

In practice, verification is often partial: some beads in a thread are done, others are in progress. The accretion model handles this gracefully via `thread_progress` returning a value in [0, 1]. The verification functor can be extended to a continuous-valued version:

**Definition 5.2 (Verification progress).** V_p: L -> [0, 1] defined as:
- V_p(x) = 1.0 if V(x) = verified
- V_p(x) = 0.0 if V(x) = failed or x has no children and is unverified
- V_p(x) = (sum of V_p(child_i)) / (number of children) for internal nodes

This is exactly `decade_progress` and `thread_progress` generalized to the full lattice.

---

## 6. The "5 Whys" Decomposition

### Why 1: Why do we need agent provenance?

Because agents make autonomous code changes. In rosary's pipeline, a dev-agent modifies source files, a staging-agent reviews them, and a prod-agent approves them. Each step produces artifacts (commits, handoffs, manifests) but there is no unified, tamper-evident record linking a specific code change back through the full decision chain to the ADR that motivated it.

### Why 2: Why is that a risk?

Because we cannot distinguish agent work from human work in the final artifact. A git commit by "dev-agent" looks structurally identical to a commit by a human developer. The dispatch manifest (`.rsry-dispatch.json`) records metadata, but it is not cryptographically bound to the commit content. An attacker could modify the commit after the fact while leaving the manifest unchanged.

### Why 3: Why does that matter?

Because supply chain attacks can inject malicious code via agent pipelines. The 2024-2025 wave of supply chain attacks (xz-utils, etc.) demonstrated that automated processes are high-value targets. Agent pipelines are automated processes that touch production code. Without provenance, a compromised agent (via prompt injection, model vulnerability, or key compromise) can inject code that appears to have gone through the full review pipeline.

### Why 4: Why can't existing tools catch this?

Because SBOMs (Software Bills of Materials) track *components* (which libraries, which versions) but not *decision chains* (why was this code written, who authorized it, what was the review process). Sigstore and in-toto track build provenance (who built the artifact and from what source) but not *development provenance* (why was this change made, what ADR motivated it, which agent executed it, what did the reviewing agent see).

### Why 5: Why is a decision chain different from a build chain?

Because it requires three properties simultaneously:

1. **Temporal ordering**: Phase 0 (dev) happened before Phase 1 (staging) happened before Phase 2 (prod). Hash chaining provides this.
2. **Causal linking**: Phase 1's review was specifically of Phase 0's output, not of some other change. Parent hash inclusion provides this.
3. **Identity binding**: The agent claiming to be "staging-agent" was actually the staging-agent, authorized by the orchestrator, running with the correct permissions. Cryptographic signatures provide this.

Existing tools provide at most two of these three. Git provides temporal ordering and (with GPG) identity binding, but not causal linking across pipeline phases. In-toto provides identity binding and causal linking for build steps, but not for the development decision chain above the build. The provenance lattice provides all three across the full hierarchy from ADR to file change.

---

## 7. Implementation Roadmap

### 7.1 Phase 1: Fix the chain hash (immediate)

Modify `Handoff` to include `previous_chain_hash: Option<[u8; 32]>` instead of (or in addition to) relying on `artifacts.previous_handoff` as the chain link. The orchestrator computes `handoff_0.chain_hash()` and passes it to `handoff_1` during construction.

**Changed file**: `src/handoff.rs`
**Bead**: `rosary-ad51f9`

**Backward compatibility**: The existing `chain_hash()` method continues to work. Add a new method `chain_hash_v2()` that uses the actual previous hash. The `previous_chain_hash` field is `Option` with `skip_serializing_if = "Option::is_none"` so old handoffs remain valid.

### 7.2 Phase 2: Bead-level hash (short term)

Add `H(bead)` computation: after the pipeline completes for a bead, compute SHA256 over all phase hashes and store it in the dispatch record or bead metadata.

**Changed files**: `src/store.rs` (add `provenance_hash` to `DispatchRecord`), `src/handoff.rs` (add `bead_hash()` method)

### 7.3 Phase 3: Thread and decade hashes (medium term)

When accretion marks a thread or decade as complete, compute `H(thread)` and `H(decade)` from their children. Store in `ThreadRecord` and `DecadeRecord`.

**Changed files**: `src/store.rs` (add `provenance_hash` fields), `crates/bdr/src/accrete.rs` (compute hash on completion)

### 7.4 Phase 4: Attestation signatures (requires signet integration)

Integrate with `signet` for Ed25519 key management. Each agent execution produces a signed attestation. The orchestrator co-signs as a witness.

**Dependency**: signet key exchange infrastructure

### 7.5 Phase 5: Tool call and file change hashing (long term)

Extend the stream JSON parser to hash individual tool calls and file changes. This is the most granular level and produces the most data. Consider whether the full granularity is needed or whether phase-level attestation is sufficient for most use cases.

---

## 8. Formal Summary

**The provenance lattice L** is a bounded graded poset with 8 grades (ADR through FileChange), extended to a lattice by adjoining top and bottom elements. The hierarchy is tree-structured by design (single-parent containment), which guarantees lattice properties.

**The provenance functor P: Prov -> Att** maps each lattice element to a signed attestation, preserving the partial order. If a <= b, then P(a)'s hash is reachable from P(b). This is a covariant functor from the poset category to the category of hash-linked attestations.

**The verification functor V** is conceptually contravariant: verification at a higher grade implies verification at all lower grades. In practice, it is implemented as bottom-up accretion (completion events flow upward) with the invariant that downward implications hold.

**The hash chain** extends the existing `Handoff::chain_hash()` to the full lattice. The critical fix is replacing path-based linking with hash-based linking. The resulting structure is a Merkle DAG where each node's hash commits to all its descendants.

**The adversarial model** identifies four threat levels. The lattice provides tamper evidence, replay detection, ordering enforcement, and phantom bead detection. Residual risks (key compromise, colluding agents, semantic correctness) require orthogonal mitigations.

**The "5 Whys"** establish that agent provenance is necessary because autonomous code changes require temporal ordering + causal linking + identity binding, a combination not provided by existing SBOM or build provenance tools.
