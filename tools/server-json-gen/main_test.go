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
	if p.Identifier != "ghcr.io/agentic-research/mache" {
		t.Errorf("identifier = %q, want ghcr.io/agentic-research/mache", p.Identifier)
	}
	if p.Version != serverVersion {
		t.Errorf("version = %q, want serverVersion %q (single-sourced from buildinfo)", p.Version, serverVersion)
	}
}
