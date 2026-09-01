package mcpserve

import (
	"sort"
	"testing"
)

func TestFilterDirIDsByKind_EmptyKindIsIdentity(t *testing.T) {
	in := []string{"go/functions/Foo", "go/methods/Bar.Baz"}
	out, ok := filterDirIDsByKind(in, "")
	if !ok {
		t.Fatalf("empty kind should be accepted; got ok=false")
	}
	if len(out) != len(in) {
		t.Fatalf("empty kind should return inputs unchanged; got %v want %v", out, in)
	}
}

func TestFilterDirIDsByKind_KnownKindFilters(t *testing.T) {
	in := []string{
		"go/functions/Foo/source",
		"go/methods/Bar.Baz/source",
		"rust/types/MyStruct/source",
		"go/functions/Quux/source",
	}
	cases := []struct {
		kind string
		want []string
	}{
		{"function", []string{"go/functions/Foo/source", "go/functions/Quux/source"}},
		{"method", []string{"go/methods/Bar.Baz/source"}},
		{"type", []string{"rust/types/MyStruct/source"}},
		{"constant", nil},
		{"variable", nil},
		{"import", nil},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			got, ok := filterDirIDsByKind(in, tc.kind)
			if !ok {
				t.Fatalf("kind %q should be accepted; got ok=false", tc.kind)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("kind %q: len=%d want=%d (got=%v want=%v)", tc.kind, len(got), len(tc.want), got, tc.want)
			}
			sort.Strings(got)
			sort.Strings(tc.want)
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("kind %q index %d: got %q want %q", tc.kind, i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestFilterDirIDsByKind_UnknownKindReturnsNotOK(t *testing.T) {
	got, ok := filterDirIDsByKind([]string{"go/functions/Foo"}, "wibble")
	if ok {
		t.Fatalf("unknown kind should be rejected; got ok=true (filtered=%v)", got)
	}
	if got != nil {
		t.Fatalf("unknown kind should return nil; got %v", got)
	}
}

func TestFilterDirIDsByKind_PluralBoundaryGuard(t *testing.T) {
	// "/functions/" must not match "/functionsBefore/" or similar near-collisions.
	in := []string{
		"go/functions/Foo",
		"go/functionsBefore/Bar",
		"functions/NotInPackage",
		"prefix/functions/Yes",
	}
	got, ok := filterDirIDsByKind(in, "function")
	if !ok {
		t.Fatalf("function kind should be accepted")
	}
	want := []string{"go/functions/Foo", "prefix/functions/Yes"}
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("index %d: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestSupportedKinds_CoversAllKindMap(t *testing.T) {
	got := supportedKinds()
	if len(got) != len(kindToPathSegment) {
		t.Fatalf("supportedKinds returned %d names; kindToPathSegment has %d", len(got), len(kindToPathSegment))
	}
	for _, k := range got {
		if _, ok := kindToPathSegment[k]; !ok {
			t.Errorf("supportedKinds returned %q not in kindToPathSegment", k)
		}
	}
}
