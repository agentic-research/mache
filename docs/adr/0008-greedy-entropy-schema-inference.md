# ADR 0008: Greedy Entropy-Based Schema Inference

## Status
Implemented

## Context
The current Formal Concept Analysis (FCA) based inference (`NextClosure` algorithm, ADR-0005) provides theoretically complete concept lattices but suffers from $O(2^m)$ complexity where $m$ is the number of attributes. For datasets with many attributes or distinct values, this is intractable without heavy sampling.

Furthermore, strict lattice generation doesn't always produce the most *navigable* directory structure for a filesystem. Users often prefer intuitive hierarchies (e.g., Temporal sharding `Year/Month/Day`) over mathematically precise but deep concept lattices.

For specific domains like Git object graphs (ADR-0007), pure entropy-based inference can fail to produce usable trees (picking high-entropy SHAs as root directories) without semantic guidance.

## Decision
Implement a **Greedy Entropy-Based Partitioning** algorithm (similar to ID3/C4.5 decision trees) as the default schema inference engine, enhanced with **Semantic Hints**.

### The Algorithm
The algorithm recursively builds a topology tree by:
1.  **Analyzing Fields:** Computing statistics (cardinality, count, type) for all fields in the current record set.
2.  **Candidate Selection:** Identifying potential split attributes.
    *   Fields with cardinality > 1.
    *   Virtual fields derived from hints (e.g., `timestamp:year`, `timestamp:month`).
3.  **Scoring:** Selecting the best attribute to split on by maximizing a weighted score:
    *   **Structural Gain:** Reduction in schema signature entropy (primary objective).
    *   **Intrinsic Entropy:** Distributional uniformity (secondary objective, for sharding uniform data).
    *   **Hint Boost:** Heavy weighting for user-hinted attributes (e.g., `temporal`).
4.  **Partitioning:** Splitting records into subsets based on the chosen attribute.
5.  **Recursion:** Repeating until max depth is reached, entropy is minimized, or record count is low.

### Semantic Hints
To address the "Git Problem" (high entropy IDs vs low entropy timestamps), we introduce a `Hints` configuration:
*   `id`: Suppresses a field from being a split candidate (prevents fragmentation).
*   `temporal`: Activates virtual candidates (`year`, `month`, `day`) and boosts their selection score to ensure hierarchical time-based sharding.

### Tie-Breaking
When multiple attributes offer similar gain, the algorithm prioritizes:
1.  **Support:** Attributes present in more records (minimizing `null` partitions).
2.  **Cardinality:** Attributes with fewer distinct values (minimizing directory clutter).

## Consequences

### Positive
*   **Performance:** Complexity reduced to $O(N \cdot M \cdot \log N)$. Inference is near-instant for thousands of records.
*   **Navigability:** Produces shallower, wider trees that map better to human mental models (especially with temporal sharding).
*   **Control:** Hints allow users to guide the inference without defining the full schema manually.
*   **Scalability:** Handles large uniform datasets (logs, commits) by falling back to intrinsic entropy partitioning.

### Negative
*   **Completeness:** Unlike FCA, greedy inference finds *one* good tree, not *all* valid concepts. Some subtle overlapping relationships might be missed.
*   **Configuration:** Optimal results for complex data (like Git) require passing hints (`--hint ts=temporal`), whereas FCA tries to find structure blindly.

## References
*   Quinlan, J.R. (1986). "Induction of Decision Trees" (ID3)
*   ADR-0005: FCA Schema Inference (Previous approach, retained as fallback)
