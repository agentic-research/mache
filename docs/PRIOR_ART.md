# Prior Art & Landscape

## Introduction

Mache is a **projection engine**: it takes structured data (JSON, SQLite, source code) and mounts it as a real POSIX filesystem. A declarative schema defines the topology — how source nodes map to directories and files — or, with `--infer`, the schema is derived automatically via Formal Concept Analysis (FCA). The engine handles ingestion, on-demand content resolution, cross-references, and write-back.

This positions Mache at the intersection of several traditions — filesystem-as-interface (Plan 9), data virtualization (FUSE-DB tools), and AI-agent context engineering — but no existing tool combines all of its properties: schema-driven projection, AST decomposition, identity-preserving write-back, and a real OS-level mount.

## Comparison Table

| Tool | Schema-Driven Projection | AST-Aware Decomposition | Identity-Preserving Write-Back | On-Demand Content | Cross-References | Real FS Mount |
|------|:---:|:---:|:---:|:---:|:---:|:---:|
| **Mache** | Yes | Yes | Yes | Yes | Yes (`callers/`, `callees/`) | Yes (NFS/FUSE) |
| **AgentFS** (Turso) | No | No | Yes (KV store) | No | No | No |
| **Dust** | No | No | No | No | No | No (synthetic FS via tool calls) |
| **Vercel bash-tool** | No | No | No | No | No | No (manual file staging) |
| **MCP** | No | No | Varies by server | N/A | N/A | No (protocol layer) |
| **LlamaIndex / LangChain** | No | No | No | Yes (retrievers) | No | No |
| **AIGNE / AFS** | No | No | No | No | No | No (namespace metaphor) |
| **FUSE-DB tools** (FusqlFS, DBFS, wddbfs) | No | No | Some | Yes | No | Yes |
| **Plan 9 / 9P** | Yes (per-server) | No | Yes | Yes | No | Yes |

## Detailed Analysis

### AgentFS (Turso)

[AgentFS](https://github.com/tursodatabase/agentfs) provides a filesystem-like key-value store for AI agents backed by libSQL/Turso. It solves agent state persistence — saving and retrieving files that agents create during execution. It does not project external data into a filesystem; there is no schema, no topology reshaping, and no AST awareness. The "filesystem" is a storage abstraction, not an OS mount.

### "FUSE is All You Need" (Emmerling, 2025)

This [blog post](https://blog.philipmemmerling.com/fuse-is-all-you-need/) articulates the philosophical argument that FUSE filesystems are the ideal interface for AI agents — tools already speak files, so mount your data as files. Mache shares this philosophy entirely but goes further: it provides a general-purpose engine with declarative schemas, multi-format ingestion, and write-back, rather than a one-off FUSE implementation for a specific data source.

### Dust

[Dust](https://dust.tt/) provides AI assistants with access to company data through tool calls that return file-like results. The "filesystem" is synthetic — constructed per-query by the orchestration layer, not mounted as a real directory tree. There is no persistent topology, no schema-driven projection, and no write-back.

### Vercel bash-tool

Vercel's approach to agent file access uses a sandboxed bash environment where files are staged manually into the agent's working directory. This gives agents real file operations but requires explicit file preparation — there is no projection engine to reshape data, no schema, and no on-demand content resolution. The agent sees whatever files were placed in its sandbox.

### MCP (Model Context Protocol)

[MCP](https://modelcontextprotocol.io/) is a protocol standard for connecting AI models to data sources and tools. It defines the transport layer (JSON-RPC, stdio/SSE) but not the data plane. An MCP server *could* expose a filesystem, but MCP itself provides no schema language, no ingestion engine, and no topology projection. Mache could be wrapped as an MCP server, but it operates at a different layer — MCP is plumbing, Mache is the projection engine.

### LangChain / LlamaIndex

These frameworks provide RAG (Retrieval-Augmented Generation) pipelines: ingest documents, chunk them, embed them, and retrieve relevant fragments at query time. They solve the retrieval problem — finding relevant context — but not the structural problem. There is no filesystem interface, no schema-driven topology, and no write-back. An agent using LlamaIndex gets text chunks; an agent using Mache gets a navigable directory tree with cross-references.

### AIGNE / AFS (Agent Filesystem)

AIGNE's "Agent Filesystem" uses filesystem metaphors for agent tool registration — tools are "mounted" at namespace paths. This is a naming convention for tool dispatch, not a real filesystem. There are no inodes, no directory traversal, no read/write operations at the OS level.

### FUSE-DB Tools (FusqlFS, DBFS, wddbfs)

These tools mount databases as real FUSE filesystems — tables become directories, rows become files. They provide genuine OS-level mounts with read (and sometimes write) support. However, they **mirror** the database schema directly: the filesystem topology is 1:1 with the source schema. Mache adds a projection layer — the topology schema reshapes data into task-appropriate structures (e.g., temporal sharding by year/month, AST decomposition into function directories) that may differ entirely from the source layout.

### Plan 9 / 9P

Plan 9 from Bell Labs is the closest philosophical ancestor. Its core principle — "everything is a file server" — directly inspires Mache's approach. In Plan 9, every resource (network, process table, window system) is exposed as a synthetic filesystem via the 9P protocol. Each server defines its own namespace topology.

Mache applies this idea to structured data with two modern additions: (1) declarative schemas that specify the projection without writing a custom file server, and (2) AST-aware decomposition that understands source code structure. Plan 9 required implementing a new file server for each data source; Mache requires only a JSON schema.

## Academic Validation

Two recent papers provide independent validation of the file-as-interface approach for AI agents:

### "Files Are All You Need" (Piskala, January 2026)

This paper argues that the filesystem is the natural interface between AI agents and structured data — agents already operate on files, so exposing data as files eliminates the impedance mismatch between data access patterns and agent tool interfaces. The paper surveys existing approaches and concludes that real OS-level mounts (not synthetic file metaphors) are essential for seamless agent integration. Mache's architecture aligns directly with this thesis.

### "Structured Context Engineering" (McMillan, February 2026)

Based on 9,649 experiments across multiple LLMs, this paper demonstrates that domain-partitioned file schemas significantly improve agent task performance compared to flat file dumps or RAG retrieval. The key finding: when data is organized into semantically meaningful directory hierarchies (by domain, by time period, by construct type), agents navigate more efficiently and produce more accurate results. This validates Mache's schema-driven topology approach — the projection is not just convenient, it materially affects agent performance.

## Sources

- AgentFS (Turso): https://github.com/tursodatabase/agentfs
- "FUSE is All You Need": https://blog.philipmemmerling.com/fuse-is-all-you-need/
- Dust: https://dust.tt/
- Model Context Protocol: https://modelcontextprotocol.io/
- LlamaIndex: https://www.llamaindex.ai/
- LangChain: https://www.langchain.com/
- FusqlFS: https://github.com/jking/fusqlfs
- Plan 9 from Bell Labs: https://9p.io/plan9/
- 9P Protocol: https://9p.io/sys/man/5/INDEX.html
