package ingest

import (
	"testing"
)

func TestDig_NestedMapPresent(t *testing.T) {
	vals := map[string]any{
		"item": map[string]any{
			"Vulnerability": map[string]any{
				"NamespaceName": "alpine:3.18",
			},
		},
	}
	got, err := RenderTemplate(`{{dig "item.Vulnerability.NamespaceName" .}}`, vals)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if got != "alpine:3.18" {
		t.Errorf("got %q, want alpine:3.18", got)
	}
}

func TestDig_NestedMapMissing(t *testing.T) {
	// OSV record — no .item.Vulnerability at all
	vals := map[string]any{
		"item": map[string]any{
			"id":       "ALBA-2019:0973",
			"affected": []any{},
		},
	}
	got, err := RenderTemplate(`{{dig "item.Vulnerability.NamespaceName" .}}`, vals)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty string for missing path, got %q", got)
	}
}

func TestDig_TopLevelKey(t *testing.T) {
	vals := map[string]any{"name": "hello"}
	got, err := RenderTemplate(`{{dig "name" .}}`, vals)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if got != "hello" {
		t.Errorf("got %q, want hello", got)
	}
}

func TestDig_ArrayIndex(t *testing.T) {
	vals := map[string]any{
		"item": map[string]any{
			"affected": []any{
				map[string]any{
					"package": map[string]any{
						"ecosystem": "AlmaLinux:8",
					},
				},
			},
		},
	}
	got, err := RenderTemplate(`{{dig "item.affected.0.package.ecosystem" .}}`, vals)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if got != "AlmaLinux:8" {
		t.Errorf("got %q, want AlmaLinux:8", got)
	}
}

func TestDig_ArrayIndexOutOfBounds(t *testing.T) {
	vals := map[string]any{
		"item": map[string]any{
			"affected": []any{},
		},
	}
	got, err := RenderTemplate(`{{dig "item.affected.0.package.ecosystem" .}}`, vals)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty for out-of-bounds, got %q", got)
	}
}

func TestDig_NilIntermediate(t *testing.T) {
	vals := map[string]any{"item": nil}
	got, err := RenderTemplate(`{{dig "item.foo.bar" .}}`, vals)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty for nil intermediate, got %q", got)
	}
}

func TestDig_CombinedWithDefaultAndReplace(t *testing.T) {
	// Vunnel record
	vunnel := map[string]any{
		"item": map[string]any{
			"Vulnerability": map[string]any{
				"NamespaceName": "alpine:3.18",
			},
		},
	}
	got, err := RenderTemplate(
		`{{replace (default (dig "item.Vulnerability.NamespaceName" .) (dig "item.affected.0.package.ecosystem" .)) ":" " "}}`,
		vunnel,
	)
	if err != nil {
		t.Fatalf("render vunnel: %v", err)
	}
	if got != "alpine 3.18" {
		t.Errorf("vunnel: got %q, want 'alpine 3.18'", got)
	}

	// OSV record
	osv := map[string]any{
		"item": map[string]any{
			"id": "ALBA-2019:0973",
			"affected": []any{
				map[string]any{
					"package": map[string]any{
						"ecosystem": "AlmaLinux:8",
					},
				},
			},
		},
	}
	got, err = RenderTemplate(
		`{{replace (default (dig "item.Vulnerability.NamespaceName" .) (dig "item.affected.0.package.ecosystem" .)) ":" " "}}`,
		osv,
	)
	if err != nil {
		t.Fatalf("render osv: %v", err)
	}
	if got != "AlmaLinux 8" {
		t.Errorf("osv: got %q, want 'AlmaLinux 8'", got)
	}
}
