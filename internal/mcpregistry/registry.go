// Package mcpregistry is the single source of truth for the names of the
// MCP tools mache exposes and their partitioning into cloister-resolver
// backend groups.
//
// Two consumers depend on this package:
//
//   - cmd/serve_handlers.go registers each tool with the mark3labs/mcp-go
//     server. A drift test (registry_drift_test.go) compares the names
//     here against the names registered there so adding a tool in one
//     place without the other fails CI.
//
//   - tools/server-json-gen emits server.json at the repo root. The
//     `_meta.art.cloister/v1.groups[]` block in that artifact is
//     generated from CloisterGroups(); the wire contract is the same
//     one ley-line-open ships under bead `ley-line-open-f10abb`.
//
// Coverage policy: every tool returned by ToolRegistry() MUST appear in
// exactly one group's UpstreamNames. The generator and a unit test both
// enforce this.
//
// See bead `mache-802d2b` for the Phase 4b cloister/mache rollout.
package mcpregistry

// Tool is a static description of one MCP tool name mache exposes.
// Only the name is canonical here — full descriptions and JSON schemas
// stay co-located with the handler registration in cmd/serve_handlers.go.
// This package owns the *partitioning* into cloister groups, not the
// per-tool schema.
type Tool struct {
	// Name is the bare tool name on the wire (no `mache_` prefix). The
	// cloister-side router rewrites between the advertised prefix
	// (`mache_*`) and the bare upstream name per the
	// cloister/cloister-spec/mcp-tool/v1 contract.
	Name string

	// Tier records which ley-line-open artifacts a tool's backend needs
	// beyond the base `_ast` projection. Surfaced in server.json's
	// `io.github.agentic-research.mache/tools` block; informational only —
	// the cloister wire ignores it.
	//
	// The empty value means TierBase, so entries that need nothing beyond
	// the parse output stay declaration-free.
	Tier Tier
}

// Tier names the ley-line-open artifact a tool depends on.
//
// Before v0.18.0 this was a bool named RequiresLeyLineOpen, splitting the
// surface into "standalone" and "requires-ley-line-open". ADR-0012 step 4
// removed the in-process parser and made `leyline parse` the sole source
// parser, at which point *every* source projection requires ley-line-open
// and "standalone" described nothing. The meaningful question became which
// additional artifact a tool needs, which is what these tiers record.
type Tier string

const (
	// TierBase needs only the `_ast` tables that `leyline parse` produces
	// for any source projection. This is the zero value.
	TierBase Tier = "base"

	// TierLSP needs the `_lsp_*` tables from leyline's LSP pass, either
	// pre-baked into a .db or produced on demand by the daemon.
	TierLSP Tier = "lsp"

	// TierEmbeddings needs embedding vectors in a ley-line-open-built .db.
	TierEmbeddings Tier = "embeddings"

	// TierAny is for tools that read no projected tables at all and are
	// therefore safe to call against any source (e.g. daemon status).
	TierAny Tier = "any"
)

// Resolved returns the tier, mapping the zero value to TierBase.
func (t Tier) Resolved() Tier {
	if t == "" {
		return TierBase
	}
	return t
}

// ToolRegistry returns the canonical, ordered list of MCP tools mache
// exposes over its `mache serve` MCP transports. Order is the wire
// order shown to clients in tools/list and the array order in
// server.json's tool listings — keep additions stable.
//
// When adding a tool: add an entry here, register it in
// cmd/serve_handlers.go, claim it in CloisterGroups(), and run
// `task gen:server-json` to regenerate server.json. The drift gate
// (`task gen:server-json:check`) and the coverage test both refuse to
// pass with a missing entry.
func ToolRegistry() []Tool {
	return []Tool{
		{Name: "list_directory"},
		{Name: "read_file"},
		{Name: "find_callers"},
		{Name: "find_callees"},
		{Name: "search"},
		{Name: "semantic_search", Tier: TierEmbeddings},
		{Name: "get_communities"},
		{Name: "find_definition"},
		{Name: "get_type_info", Tier: TierLSP},
		{Name: "get_diagnostics", Tier: TierLSP},
		{Name: "get_overview"},
		{Name: "get_sheaf_status", Tier: TierAny},
		{Name: "get_impact"},
		{Name: "get_dataflow"},
		{Name: "get_architecture"},
		{Name: "get_diagram"},
		{Name: "write_file"},
		{Name: "resolve_ref"},
		{Name: "find_smells"},
	}
}

// CloisterGroupDecl is one entry in the
// `_meta.art.cloister/v1.groups[]` block emitted into server.json.
// Mirrors the wire shape described in
// cloister/cloister-spec/mcp-tool/v1/wire/meta-groups.md.
type CloisterGroupDecl struct {
	// Name is the operator-facing group identifier (also a routable
	// backend name on the cloister side). Must be non-empty.
	Name string

	// AdvertisedPrefix is the prefix cloister advertises this group's
	// tools under to its downstream clients. Mache's tools all reach
	// cloister under the `mache_` prefix today (see cloister's
	// cluster.toml `handlesPrefix = "mache_"` declaration), so every
	// group uses the same `mache_` prefix unless a future bundle
	// re-partitions.
	AdvertisedPrefix string

	// UpstreamNames are the bare tool names this group claims, in
	// stable wire order. Must be non-empty and every entry must
	// correspond to a Name in ToolRegistry().
	UpstreamNames []string
}

// CloisterGroups partitions ToolRegistry() into operator-facing
// cloister backend groups. The split mirrors mache's own functional
// surface: navigation (browse + summarize), callgraph (refs/defs/impact),
// lsp (LSP-enriched tools needing ley-line-open), lifecycle (daemon
// status), linter (find_smells), and mutate (write_file).
//
// Every tool in ToolRegistry() must appear in exactly one group; the
// generator (tools/server-json-gen) and the coverage unit test both
// enforce this.
func CloisterGroups() []CloisterGroupDecl {
	return []CloisterGroupDecl{
		// Navigation — browsing the projected tree, top-down
		// orientation, and quotient-graph views. All read-side.
		{
			Name:             "navigation",
			AdvertisedPrefix: "mache_",
			UpstreamNames: []string{
				"list_directory",
				"read_file",
				"get_overview",
				"get_architecture",
				"get_diagram",
				"get_communities",
			},
		},
		// Callgraph — symbol-centric queries (defs/refs/impact) and
		// pattern search. The bread-and-butter code-intelligence
		// surface.
		{
			Name:             "callgraph",
			AdvertisedPrefix: "mache_",
			UpstreamNames: []string{
				"find_callers",
				"find_callees",
				"find_definition",
				"get_impact",
				"get_dataflow",
				"search",
				"resolve_ref",
			},
		},
		// LSP — tools that read LSP enrichment tables produced by
		// ley-line-open. Separate group so a deployment without LLO
		// can route this backend to a stub or 503 it explicitly.
		{
			Name:             "lsp",
			AdvertisedPrefix: "mache_",
			UpstreamNames: []string{
				"get_type_info",
				"get_diagnostics",
				"semantic_search",
			},
		},
		// Lifecycle — daemon status surface. Single-tool group; lives
		// on its own so a probe wired to it can be cheap and never
		// fall through to the heavier handlers.
		{
			Name:             "lifecycle",
			AdvertisedPrefix: "mache_",
			UpstreamNames:    []string{"get_sheaf_status"},
		},
		// Linter — structural code-smell rules over the parsed AST.
		// Its own group because find_smells has substantially
		// different cost characteristics (runs SQL over _ast) than
		// the navigation/callgraph reads.
		{
			Name:             "linter",
			AdvertisedPrefix: "mache_",
			UpstreamNames:    []string{"find_smells"},
		},
		// Mutate — the write-back surface. Isolated so a policy can
		// allow reads and deny writes by routing this group to a
		// 403-only backend without touching the others.
		{
			Name:             "mutate",
			AdvertisedPrefix: "mache_",
			UpstreamNames:    []string{"write_file"},
		},
	}
}
