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
	// ADR-0041 canonical form: identifier is TAGLESS (the image name); version is
	// the git tag (v-prefixed, matching release.yml's `:${TAG}`). cloister resolves
	// tag→digest and pins identifier@digest. serverVersion single-sourced from buildinfo.
	if p.Identifier != "ghcr.io/agentic-research/mache" {
		t.Errorf("identifier = %q, want tagless ghcr.io/agentic-research/mache (ADR-0041)", p.Identifier)
	}
	wantVer := "v" + serverVersion
	if p.Version != wantVer {
		t.Errorf("version = %q, want %q (the git tag; ADR-0041 — cloister resolves tag→digest)", p.Version, wantVer)
	}
}
