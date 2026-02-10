# 2. Declarative Topology Schema

Date: 2026-02-10
Status: Accepted

## Context

Hardcoding filesystem logic (e.g., `if path == "/hello"`) scales poorly.
We need a way to map arbitrary input data (JSON, Git objects, APIs) to a filesystem hierarchy without recompiling the binary.
The mapping must support:

1. **Static Navigation:** Fixed paths (e.g., `/vulns`).
2. **Dynamic Projection:** Creating directories from data arrays (e.g., `/vulns/{cve_id}`).
3. **Data Extraction:** Reading fields from the source to populate file content.

## Decision

We will implement a **Declarative Schema Engine** driven by a `Topology` configuration.

### The Schema Structure

The file system is defined as a tree of `Nodes` (directories) and `Leaves` (files).

```go
type Node struct {
    Name     string // Template allowed: "{{.id}}"
    Selector string // Query to scope data: "vulns.#"
    Children []Node // Recursive structure
    Files    []Leaf // Files in this directory
}

type Leaf struct {
    Name            string // Template allowed: "severity"
    ContentTemplate string // Template allowed: "{{.severity}}"
}
```

### The Engine Mechanics

1. **Eager Loading (MVP):** At mount time, we walk the `Topology` and the `Input Data` simultaneously to build an in-memory `Inode` tree.
2. **Templating:** We use Go's `text/template` for field injection.
3. **Querying:** We use a path syntax (e.g., `buger/jsonparser` or `GJSON`) to slice the input data for child nodes.

## Consequences

- **Strict Separation:** The Go code (Engine) never knows the shape of the data. It only knows how to follow the Schema.
- **Read-Only:** This design assumes a read-only projection. Writing back to the source (reverse-templating) is out of scope for v1.
- **Performance:** Eager loading requires parsing the full dataset at startup.
  - *Mitigation:* We will move to "Just-In-Time" (Lazy) Inode creation in v2, where `Readdir` triggers the parsing for that specific branch.
