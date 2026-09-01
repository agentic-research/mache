// Tests for the extractEnrichSkipReasons + formatNoHoverMessage helpers
// — the consumer surface of leyline-v0.5.3's `EnrichmentStats.skipped`
// wire field (bead ley-line-open-661727).
//
// The helpers fail closed: pre-v0.5.3 daemons drop `skipped` at the
// capnp boundary so the response carries no field; helpers return nil,
// and callers fall back to the prior "no hover info found" message.

package mcpserve

import (
	"strings"
	"testing"
)

func TestExtractEnrichSkipReasons_PopulatedPasses(t *testing.T) {
	// Shape of a leyline-v0.5.3 enrich response: passes is a JSON array
	// of {pass_name, files_processed, items_added, duration_ms, skipped}
	// objects. The Go capnp-JSON decoder maps each pass entry to
	// map[string]any.
	resp := map[string]any{
		"ok":           true,
		"current_root": "abc123",
		"passes": []any{
			map[string]any{
				"pass_name":       "tree-sitter",
				"files_processed": uint64(1),
				"items_added":     uint64(25),
				"duration_ms":     uint64(5),
				"skipped":         []any{},
			},
			map[string]any{
				"pass_name":       "lsp",
				"files_processed": uint64(1),
				"items_added":     uint64(0),
				"duration_ms":     uint64(150),
				"skipped": []any{
					"language server 'rust-analyzer' not on PATH for language 'rust' (1 file(s) skipped)",
				},
			},
		},
	}

	got := extractEnrichSkipReasons(resp)
	if len(got) != 1 {
		t.Fatalf("expected exactly one skip reason, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "[lsp]") {
		t.Errorf("skip reason must prefix with pass_name; got: %s", got[0])
	}
	if !strings.Contains(got[0], "rust-analyzer") {
		t.Errorf("skip reason must carry the underlying daemon message; got: %s", got[0])
	}
}

func TestExtractEnrichSkipReasons_NoSkippedField(t *testing.T) {
	// Pre-v0.5.3 daemon shape: passes entries don't carry a skipped
	// field (the capnp builder dropped it). Helper returns nil so the
	// caller doesn't tack on an empty "Enrichment pass skip reasons:"
	// section.
	resp := map[string]any{
		"ok": true,
		"passes": []any{
			map[string]any{
				"pass_name":       "lsp",
				"files_processed": uint64(1),
				"items_added":     uint64(0),
				"duration_ms":     uint64(150),
			},
		},
	}
	got := extractEnrichSkipReasons(resp)
	if len(got) != 0 {
		t.Errorf("pre-v0.5.3 response (no skipped field) must return empty; got: %v", got)
	}
}

func TestExtractEnrichSkipReasons_MalformedResponse(t *testing.T) {
	// passes field missing entirely.
	if got := extractEnrichSkipReasons(map[string]any{"ok": true}); len(got) != 0 {
		t.Errorf("missing passes field must return empty; got: %v", got)
	}
	// passes is wrong type.
	if got := extractEnrichSkipReasons(map[string]any{"passes": "not a list"}); len(got) != 0 {
		t.Errorf("malformed passes must return empty; got: %v", got)
	}
}

func TestFormatNoHoverMessage_WithSkipReasons(t *testing.T) {
	reasons := []string{
		"[lsp] language server 'rust-analyzer' not on PATH for language 'rust' (1 file(s) skipped)",
		"[lsp] scope matched no _source.id rows; requested 1 file(s): [\"crates/memory-core/src/lib.rs\"]",
	}
	got := formatNoHoverMessage("Foo", "function", reasons)
	if !strings.Contains(got, "Foo") {
		t.Errorf("message must name the symbol; got: %s", got)
	}
	if !strings.Contains(got, "kind=function") {
		t.Errorf("message must name the kind when provided; got: %s", got)
	}
	if !strings.Contains(got, "Enrichment pass skip reasons:") {
		t.Errorf("message must label the skip-reason section; got: %s", got)
	}
	for _, r := range reasons {
		if !strings.Contains(got, r) {
			t.Errorf("message must include each skip reason verbatim; missing %q in: %s", r, got)
		}
	}
}

func TestFormatNoHoverMessage_NoSkipReasons(t *testing.T) {
	got := formatNoHoverMessage("Foo", "", nil)
	if strings.Contains(got, "Enrichment pass skip reasons:") {
		t.Errorf("must not include the skip-reason section when none given; got: %s", got)
	}
	if !strings.Contains(got, "Foo") {
		t.Errorf("must still name the symbol; got: %s", got)
	}
}
