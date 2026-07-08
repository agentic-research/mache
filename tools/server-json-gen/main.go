// server-json-gen emits mache's server.json artifact byte-stably from
// internal/mcpregistry — the same self-maintaining MCP Registry surface
// pattern ley-line-open ships under bead `ley-line-open-f10abb`.
//
// The Taskfile entries `gen:server-json` and `gen:server-json:check`
// regenerate and diff the committed artifact at the repo root; the CI
// workflow fails on drift. See bead `mache-802d2b` for rationale.
//
// # Coverage policy
//
// Every tool returned by mcpregistry.ToolRegistry() MUST appear in
// exactly one group's UpstreamNames. The generator enforces this at
// runtime — partial coverage exits non-zero with a message naming the
// orphan tool(s). The matching unit test
// (internal/mcpregistry.TestCloisterGroupsCoverEveryRegisteredToolExactlyOnce)
// enforces the same invariant at `task test` time so this gate is a
// belt-and-braces backstop, not the only check.
//
// # Reproducibility
//
// Two consecutive runs MUST produce byte-identical output. Field order
// is fixed by the struct definitions; array order matches the
// registry / groups declaration order; no map iteration leaks into the
// rendered artifact (every container is a slice or an ordered helper).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/agentic-research/mache/internal/buildinfo"
	"github.com/agentic-research/mache/internal/mcpregistry"
)

// schemaURL pins the MCP Registry schema version mache's server.json
// declares against. Bump when the registry ships a new dated schema
// and the wire shape needs to follow.
const schemaURL = "https://static.modelcontextprotocol.io/schemas/2025-12-11/server.schema.json"

// serverName is the canonical registry-facing identifier for this
// server. Matches the GitHub <owner>/<repo> shape registries dispatch
// on.
const serverName = "io.github.agentic-research/mache"

// serverTitle is the display name shown in registry listings.
const serverTitle = "mache"

// ociImageIdentifier is the registry path mache's release publishes its
// leyline-bundled OCI image to (see .github/workflows/release.yml image job).
// Declared in server.json packages[] so consumers (cloister ADR-0038) derive
// the bundle image from the version-pinned artifact instead of a hand-written
// tag. Tag-pinned only — digest/CAS pinning is the consumer's concern.
const ociImageIdentifier = "ghcr.io/agentic-research/mache"

// serverDescription opens the registry listing. The MCP Registry
// schema 2025-12-11 caps `description` at 100 chars — the long pitch
// the previously hand-maintained server.json carried was actually
// schema-invalid; this rewrite fits the cap while keeping the
// elevator-pitch shape. The richer marketing copy lives in
// README.md / GETTING-STARTED.md.
const serverDescription = "Project source trees, SQLite, and JSON as a structured filesystem with MCP code-intelligence tools."

// serverVersion is the mache release version stamped into server.json,
// sourced from the embedded internal/buildinfo single source of truth.
// melange.yaml's `version:` field and the OCI image tag still move by
// hand at release time, but `task version:check` asserts they agree with
// buildinfo so this stamp can no longer silently drift from the binary.
var serverVersion = buildinfo.Version

// repositoryURL + repositorySource describe where the source lives.
const (
	repositoryURL    = "https://github.com/agentic-research/mache"
	repositorySource = "github"
)

// websiteURL surfaces the operator-facing "how do I run this?" doc.
const websiteURL = "https://github.com/agentic-research/mache/blob/main/GETTING-STARTED.md"

// defaultHTTPPort is the default streamable-http port mache serves
// under. Surfaced in the `remotes[]` entry's variable defaults and
// hard-coded into the transport `args` so the registry listing is a
// drop-in copy.
const defaultHTTPPort = "7532"

// minLeyLineOpenVersion declares the minimum LLO release whose
// enrichment tables mache reads. Bumped in lock-step with LLO's
// substrate / wire-format changes that touch _ast / _lsp_* schemas.
const minLeyLineOpenVersion = "0.4.5"

// serverDoc is the top-level server.json envelope. Field order is
// load-bearing — encoding/json's struct-field ordering is the contract
// for byte-stable output, and downstream tools that diff this file
// rely on a predictable shape.
type serverDoc struct {
	Schema      string         `json:"$schema"`
	Name        string         `json:"name"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	Version     string         `json:"version"`
	Repository  repository     `json:"repository"`
	WebsiteURL  string         `json:"websiteUrl"`
	Packages    []packageEntry `json:"packages"`
	Remotes     []remote       `json:"remotes"`
	Meta        *orderedObject `json:"_meta"`
}

type repository struct {
	URL    string `json:"url"`
	Source string `json:"source"`
}

type packageEntry struct {
	RegistryType string `json:"registryType"`
	Identifier   string `json:"identifier"`
	Version      string `json:"version,omitempty"`
}

type remote struct {
	Type      string                       `json:"type"`
	URL       string                       `json:"url"`
	Variables map[string]remoteVarTemplate `json:"variables"`
}

type remoteVarTemplate struct {
	Description string `json:"description"`
	Default     string `json:"default"`
}

type transportEntry struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Note    string   `json:"note"`
}

type toolSplit struct {
	Standalone          []string `json:"standalone"`
	RequiresLeyLineOpen []string `json:"requires-ley-line-open"`
}

type optionalDep struct {
	Purpose        string `json:"purpose"`
	MinimumVersion string `json:"minimum-version"`
}

type sourceAcceptance struct {
	Directories string `json:"directories"`
	DBFiles     string `json:"db-files"`
	JSON        string `json:"json"`
	SQLite      string `json:"sqlite"`
}

type artCloisterV1 struct {
	Groups []groupOut `json:"groups"`
}

type groupOut struct {
	Name             string   `json:"name"`
	AdvertisedPrefix string   `json:"advertisedPrefix"`
	UpstreamNames    []string `json:"upstreamNames"`
}

// orderedObject preserves the insertion order of nested _meta entries
// when MarshalJSON serializes them. Go maps don't preserve insertion
// order and the encoding/json package walks struct fields in declared
// order but maps in (sorted) key order — which means
// `_meta.io.github.agentic-research.mache/...` keys would land in
// alphabetical order, not the order chosen here. Use orderedObject
// when key order matters; otherwise rely on struct-field ordering.
type orderedObject struct {
	keys   []string
	values map[string]any
}

func newOrderedObject() *orderedObject {
	return &orderedObject{values: map[string]any{}}
}

func (o *orderedObject) set(key string, value any) {
	if _, exists := o.values[key]; !exists {
		o.keys = append(o.keys, key)
	}
	o.values[key] = value
}

// MarshalJSON renders the object with its insertion-order keys, using
// each value's normal serialization. We hand-build the buffer rather
// than calling json.Marshal on a map because that would re-sort keys.
//
// HTML escaping is disabled on the inner encoder for consistency with
// the top-level encoder in main() — otherwise embedded values
// containing `<` (e.g. the transport `<source-path>` placeholder)
// would render as `<` here and as the raw character in the
// outer doc, producing inconsistent output. Encoder.Encode appends a
// trailing newline; we strip it before splicing each value in.
func (o *orderedObject) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, key := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		keyJSON, err := encodeNoHTML(key)
		if err != nil {
			return nil, fmt.Errorf("marshal key %q: %w", key, err)
		}
		buf.Write(keyJSON)
		buf.WriteByte(':')
		valJSON, err := encodeNoHTML(o.values[key])
		if err != nil {
			return nil, fmt.Errorf("marshal value for %q: %w", key, err)
		}
		buf.Write(valJSON)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// encodeNoHTML mirrors json.Marshal but with HTMLEscape disabled so
// raw `<`, `>`, `&` characters survive into the output. The trailing
// newline json.Encoder appends is trimmed.
func encodeNoHTML(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	// Encoder.Encode appends a single '\n'. Strip it so the bytes
	// splice into a larger JSON value cleanly.
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	return out, nil
}

// buildDoc assembles the full server.json document from the canonical
// registry. Validation lives here — partial cloister-group coverage
// or duplicate claims abort with a descriptive error so a stale
// regen never lands.
func buildDoc() (*serverDoc, error) {
	groupsDecl := mcpregistry.CloisterGroups()
	registry := mcpregistry.ToolRegistry()

	if err := validateCoverage(registry, groupsDecl); err != nil {
		return nil, err
	}

	// Convert internal types to the wire shape. Both lists preserve
	// their source order so regens diff cleanly.
	groups := make([]groupOut, 0, len(groupsDecl))
	for _, g := range groupsDecl {
		names := make([]string, len(g.UpstreamNames))
		copy(names, g.UpstreamNames)
		groups = append(groups, groupOut{
			Name:             g.Name,
			AdvertisedPrefix: g.AdvertisedPrefix,
			UpstreamNames:    names,
		})
	}

	// Standalone vs requires-ley-line-open split mirrors the existing
	// hand-maintained block. Drive it from the registry's
	// RequiresLeyLineOpen field so it can't drift from the truth.
	var standalone, llo []string
	for _, tool := range registry {
		if tool.RequiresLeyLineOpen {
			llo = append(llo, tool.Name)
		} else {
			standalone = append(standalone, tool.Name)
		}
	}

	meta := newOrderedObject()
	meta.set("io.github.agentic-research.mache/transports", buildTransports())
	meta.set("io.github.agentic-research.mache/tools", toolSplit{
		Standalone:          standalone,
		RequiresLeyLineOpen: llo,
	})
	meta.set("io.github.agentic-research.mache/optional-deps", buildOptionalDeps())
	meta.set("io.github.agentic-research.mache/source-acceptance", sourceAcceptance{
		Directories: "any tree containing source files — auto-detects language preset (Go, Rust, Python, TypeScript, ...)",
		DBFiles:     ".db files produced by `mache build` or `leyline parse` open instantly via SQLiteGraph (pure-Go path)",
		JSON:        "JSON corpora ingest via JSONPath walker (see examples/nvd-schema.json)",
		SQLite:      "arbitrary SQLite via templated schemas (see examples/mcp-schema.json)",
	})
	meta.set("art.cloister/v1", artCloisterV1{Groups: groups})

	doc := &serverDoc{
		Schema:      schemaURL,
		Name:        serverName,
		Title:       serverTitle,
		Description: serverDescription,
		Version:     serverVersion,
		Repository: repository{
			URL:    repositoryURL,
			Source: repositorySource,
		},
		WebsiteURL: websiteURL,
		Packages: []packageEntry{{
			RegistryType: "oci",
			// OCI packages carry the tag IN the identifier (registry/repo:tag)
			// per the pinned MCP registry schema; the standalone `version` field
			// is npm/pypi/nuget-only. The tag matches release.yml's `:${TAG}`
			// (v-prefixed), so a resolver reading identifier can actually pull it.
			Identifier: ociImageIdentifier + ":v" + serverVersion,
		}},
		Remotes: []remote{
			{
				Type: "streamable-http",
				URL:  "http://localhost:{port}/mcp",
				Variables: map[string]remoteVarTemplate{
					"port": {
						Description: fmt.Sprintf("Port where `mache serve --http :PORT` is listening (default %s)", defaultHTTPPort),
						Default:     defaultHTTPPort,
					},
				},
			},
		},
		Meta: meta,
	}
	return doc, nil
}

func buildTransports() *orderedObject {
	transports := newOrderedObject()
	transports.set("stdio", transportEntry{
		Command: "mache",
		Args:    []string{"serve", "--stdio", "<source-path>"},
		Note:    "Per-client subprocess. Use for editor integrations that own the daemon lifecycle. See .mcp.json at repo root for a Claude Code template.",
	})
	transports.set("streamable-http", transportEntry{
		Command: "mache",
		Args:    []string{"serve", "--http", ":" + defaultHTTPPort, "<source-path>"},
		Note:    "Recommended for shared use — one daemon across all clients avoids per-client FD leaks.",
	})
	return transports
}

func buildOptionalDeps() *orderedObject {
	deps := newOrderedObject()
	deps.set("io.github.agentic-research/ley-line-open", optionalDep{
		Purpose:        "Provides _ast / _lsp_* / _embeddings tables consumed by semantic_search, get_type_info, get_diagnostics, and the AST-backed find_smells rules.",
		MinimumVersion: minLeyLineOpenVersion,
	})
	return deps
}

// validateCoverage enforces the bead `mache-802d2b` policy: every tool
// in ToolRegistry() must appear in exactly one cloister group, every
// group claim must point to a real registered tool, and every group
// must have a non-empty name + non-empty claim list.
func validateCoverage(registry []mcpregistry.Tool, groups []mcpregistry.CloisterGroupDecl) error {
	registered := map[string]struct{}{}
	for _, tool := range registry {
		registered[tool.Name] = struct{}{}
	}

	owner := map[string][]string{}
	for _, g := range groups {
		if g.Name == "" {
			return fmt.Errorf("cloister group has empty `name` — spec violation")
		}
		if len(g.UpstreamNames) == 0 {
			return fmt.Errorf("cloister group %q has empty UpstreamNames — spec violation", g.Name)
		}
		for _, tool := range g.UpstreamNames {
			owner[tool] = append(owner[tool], g.Name)
		}
	}

	var orphans []string
	for tool := range registered {
		if _, ok := owner[tool]; !ok {
			orphans = append(orphans, tool)
		}
	}
	sort.Strings(orphans)
	if len(orphans) > 0 {
		return fmt.Errorf("%d tool(s) in ToolRegistry() are not claimed by any cloister group: %v",
			len(orphans), orphans)
	}

	var ghosts []string
	var overclaimed []string
	for tool, names := range owner {
		if _, real := registered[tool]; !real {
			ghosts = append(ghosts, tool)
		}
		if len(names) > 1 {
			overclaimed = append(overclaimed, fmt.Sprintf("%s -> %v", tool, names))
		}
	}
	sort.Strings(ghosts)
	sort.Strings(overclaimed)
	if len(ghosts) > 0 {
		return fmt.Errorf("%d cloister group claim(s) reference tools not in ToolRegistry(): %v",
			len(ghosts), ghosts)
	}
	if len(overclaimed) > 0 {
		return fmt.Errorf("tools claimed by multiple cloister groups: %v", overclaimed)
	}
	return nil
}

func main() {
	doc, err := buildDoc()
	if err != nil {
		fmt.Fprintf(os.Stderr, "server-json-gen: %v\n", err)
		os.Exit(1)
	}

	// json.Encoder with HTML escaping disabled keeps `<source-path>`
	// readable in the output (Go's default would render `<` as
	// `<`, which is valid JSON but human-hostile). Indent matches
	// the existing committed shape: two spaces, trailing newline.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		fmt.Fprintf(os.Stderr, "server-json-gen: encode: %v\n", err)
		os.Exit(1)
	}

	// json.Encoder.Encode appends its own trailing newline, so the
	// buffer already ends with one — no extra append needed.
	if _, err := os.Stdout.Write(buf.Bytes()); err != nil {
		fmt.Fprintf(os.Stderr, "server-json-gen: write: %v\n", err)
		os.Exit(1)
	}
}
