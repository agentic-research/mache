# ADR 0007: Git Object Graph as Filesystem Projection

Status: Proposed
Context:
Mache currently projects three types of structured data into filesystems:

JSON/SQLite records (via JsonWalker, SQLiteGraph)
Source code ASTs (via SitterWalker)
Any structure via FCA-inferred schemas

Git repositories are fundamentally graphs of content-addressed objects (commits, trees, blobs) with well-defined relationships. These objects are already immutable and content-addressed - natural fits for filesystem projection.
Decision:
Extend Mache to treat git repositories as navigable, editable graph structures exposed via FUSE, where:

Commits become directories (identified by SHA)
Parent/tree relationships become symlinks
Metadata (message, author) becomes editable files
Rebase/squash operations become filesystem operations (mv, rm, cp)

Implementation Approach:
Add internal/ingest/git_walker.go implementing the Walker interface:
gotype GitWalker struct {
    repoPath string
    objects  map[string]GitObject
}

type GitObject struct {
    Type     string   // "commit", "tree", "blob"
    SHA      string
    Parent   []string
    Tree     string
    Message  string
    Author   string
    // ... other metadata
}

func (gw *GitWalker) Query(selector string) ([]Match, error) {
    // Walk git objects, return as records
    // FCA inference discovers optimal projection
}
The existing FCA inference will automatically discover:

High-cardinality fields (SHA) → directory names
Reference fields (parent, tree) → symlinks
Content fields (message, author) → leaf files

Rationale:

Graph manipulation is core to Mache: Git operations (rebase, cherry-pick, squash) are graph rewrites - exactly what Mache's write-back pipeline handles
Immutability aligns: Git objects are content-addressed and immutable, matching Mache's SourceOrigin model
Agent workflow improvement: Complex git operations become simple file operations
Reuses existing infrastructure: No new graph engine needed - git objects are just another record type

Example Usage:
bash# Mount git repo
mache /my-repo/.git --infer /mnt

# Browse history

```shell
ls /mnt/commits/
cat /mnt/commits/abc123/message
```

# Interactive rebase (reorder commits)

```shell
mv /mnt/commits/abc123 /mnt/staging/
rm /mnt/commits/abc123/parent
ln -s ../xyz789 /mnt/commits/abc123/parent
```

# Squash commits

cat /mnt/commits/abc123/message >> /mnt/commits/def456/message
rm -rf /mnt/commits/abc123

# Cherry-pick individual changes

```shell
cp /mnt/commits/abc123/src/Calculate/source /mnt/working/
```

Consequences:
Positive:

Git operations accessible to agents without parsing git CLI output
Visual, navigable commit history
Composable with existing AST projection (view code at any commit)
Rebase/merge conflicts become BROKEN_ nodes

Negative:

Git SHA recalculation on edits (compute-intensive)
Ref invalidation cascades (editing commit invalidates all descendants)
Conflict resolution UX needs design

Related:

ADR-0002: Declarative Topology (git objects fit the schema model)
ADR-0006: Syntax-Aware Write Protection (extends to semantic correctness of git graphs)
