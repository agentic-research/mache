package main

import "testing"

// TestBuildDoc_EmitsOCIPackage asserts server.json declares its own OCI
// image source (ghcr) tag-pinned to the build version, so cloister (ADR-0038)
// can derive the bundle image instead of a hand-written, drift-prone tag.
func TestBuildDoc_EmitsOCIPackage(t *testing.T) {
	doc, err := buildDoc()
	if err != nil {
		t.Fatalf("buildDoc: %v", err)
	}
	if len(doc.Packages) != 1 {
		t.Fatalf("want exactly 1 package, got %d", len(doc.Packages))
	}
	p := doc.Packages[0]
	if p.RegistryType != "oci" {
		t.Errorf("registryType = %q, want \"oci\"", p.RegistryType)
	}
	// OCI packages carry the tag IN the identifier (registry/repo:tag) per the
	// pinned MCP schema, v-prefixed to match release.yml's `:${TAG}` so a
	// resolver can actually pull it. serverVersion is single-sourced from buildinfo.
	wantIdent := "ghcr.io/agentic-research/mache:v" + serverVersion
	if p.Identifier != wantIdent {
		t.Errorf("identifier = %q, want %q (tag embedded + v-prefixed to match the published image)", p.Identifier, wantIdent)
	}
	// The standalone `version` field is npm/pypi/nuget-only; OCI must omit it.
	if p.Version != "" {
		t.Errorf("version = %q, want empty (OCI version lives in the identifier tag)", p.Version)
	}
}
