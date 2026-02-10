# 3. Self-Organizing Learned Filesystem

Date: 2026-02-10
Status: Proposed

## Context

Machine learning models learn internal representations (embeddings) that capture semantic relationships in data. However, these representations are:

1. **Opaque:** Stored as dense vectors, not human-interpretable.
2. **Locked Inside Models:** Require model execution to access, making them expensive to query.
3. **Separate from Storage:** Training data organization is independent of learned structure, missing optimization opportunities.

Meanwhile, Mache provides a content-addressed storage system with multiple filesystem views. What if the model could **organize the filesystem during training**, externalizing its learned representation as navigable directory structure?

## Decision

We will extend Mache to support **self-organizing filesystems during model training**.

### Core Mechanism

During training, the model:
1. Learns internal representations (embeddings) via standard backpropagation.
2. Periodically reorganizes the Mache filesystem based on learned similarity.
3. Creates/updates directory structures that reflect semantic clustering.
4. Subsequent training epochs benefit from improved data locality.

### Architecture

```
Training Loop:
┌─────────────────────────────────────┐
│ Data in CAS (content-addressed)     │
│         ↓                           │
│ Model trains on batch               │
│         ↓                           │
│ Model clusters learned embeddings   │
│         ↓                           │
│ Mache reorganizes hard links        │
│ /by-concept/auth/*.json             │
│         ↓                           │
│ Next epoch reads organized data     │
└─────────────────────────────────────┘
```

### Key Properties

1. **Content-Addressed Storage (CAS):** Actual data stays in `/data/cas/sha256:*`. Never moves.
2. **Hard Links for Views:** Organization is just hard links. Zero duplication, O(1) reorganization.
3. **Externalized Latent Space:** Directory structure = model's learned concept hierarchy.
4. **Inference = Navigation:** Finding similar items = `ls` the right directory.

### Example: Vulnerability Data

```bash
# Initial state (flat)
/data/cas/sha256:abc.json
/data/cas/sha256:def.json

# After Epoch 1: Model creates coarse clusters
/learned/security-concepts/
  cluster-0/  # Model learned these are similar
    CVE-2024-1234.json -> /data/cas/sha256:abc
    CVE-2024-5678.json -> /data/cas/sha256:def

# After Epoch 5: Model refines into semantic subclusters
/learned/security-concepts/
  authentication/
    jwt/
      CVE-2024-1234.json
    oauth/
      CVE-2024-5678.json
```

### Example: Vision Training Data

```bash
# Model organizes images as it learns
/training-data/learned-concepts/
  animals/
    cats/
      tabby/*.jpg
      siamese/*.jpg
    dogs/*.jpg
  vehicles/
    cars/*.jpg

# DataLoader just reads directories
# "Nearby" files are semantically similar
# Better batch diversity, faster convergence
```

## Integration with BREAD

This design builds on the BREAD framework (Bundles, Relations, Embeddings, And Dimensions):

- **Hyperbolic Core:** Directory depth = concept hierarchy, naturally matching learned tree structures.
- **Fiber Bundles:** Each file retains multi-aspect metadata (labels, EXIF, etc.) independent of placement.
- **Simplicial Complexes:** Files can appear in multiple views (hard links), modeling many-to-many relationships.
- **SheafCell Architecture:** Ensures consistency when files appear in multiple organizational schemes.

BREAD proves this is theoretically sound. This ADR proposes making it trainable.

## Hybrid with SQLite

For complex queries beyond filesystem navigation:

```python
# Simple query: Filesystem navigation (O(k))
similar_vulns = os.listdir("/learned/authentication/jwt/")

# Complex query: SQLite virtual table
db.execute("""
    SELECT * FROM vulns
    WHERE learned_cluster = 'authentication'  -- Navigate to directory
      AND cvss > 8.0                          -- Filter in-memory
      AND published_date > '2024-01-01'
""")
```

The SQLite vtable knows to navigate learned directories first (cheap), then filter (expensive operations only on subset).

## Consequences

### Positive

1. **Interpretable Representations:** `tree /learned-structure/` shows what the model learned.
2. **No Vector Database:** Inference = directory navigation, not ANN search.
3. **Continual Learning:** New data placed in existing structure or creates new clusters.
4. **Multi-Modal Naturally:** Vision, text, audio models share same filesystem organization.
5. **Curriculum Learning Built-In:** Directory structure becomes more refined each epoch.
6. **Distributed Training:** Workers organize subsets, sync via rsync/git.
7. **Versionable Knowledge:** `git diff` on learned structure shows concept drift.

### Negative

1. **Filesystem Overhead:** Large-scale reorganization (millions of hard links) may stress filesystem.
2. **Update Frequency Trade-off:** Reorganize too often = I/O overhead. Too rarely = stale organization.
3. **Novel Research Area:** No existing implementations or benchmarks.
4. **Model Complexity:** Model must learn both task objective AND organizational structure.

### Open Questions

1. **Reorganization Strategy:** Hierarchical clustering? Attention weights? Learned prototypes?
2. **Update Frequency:** Every epoch? Every N batches? Continuous?
3. **Convergence Guarantees:** Does filesystem organization help or hurt training dynamics?
4. **Scalability:** Can this work with billions of items, or limited to millions?

## Implementation Plan

### Phase 1: Static Post-Training Organization (v0.1)
- Train model normally
- After convergence, cluster embeddings
- Build Mache views from clusters
- Validate: Does filesystem organization match learned structure?

### Phase 2: Periodic Reorganization (v0.2)
- Hook into training loop
- Reorganize filesystem every N epochs
- Measure: Does data locality improve convergence?

### Phase 3: Continuous Co-Learning (v1.0)
- Model and filesystem co-evolve during training
- Real-time updates to directory structure
- Measure: Training speed, interpretability, inference cost

## Success Criteria

This approach succeeds if:
1. **Interpretability:** Humans can understand learned concepts by browsing directories.
2. **Performance:** Inference via filesystem navigation is faster than vector DB queries.
3. **Training Benefit:** Organized data improves convergence speed or model quality.
4. **Practical Scale:** Works with real-world datasets (millions of items).

## References

- BREAD paper: Theoretical foundation for multi-view graph organization
- ADR-0002: Declarative topology schema (basis for learned organization)
- Grype use case: Vulnerability data as motivating example
