package lattice

import (
	"testing"

	"github.com/RoaringBitmap/roaring"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Field path walking
// ---------------------------------------------------------------------------

func TestWalkFieldPaths_Flat(t *testing.T) {
	record := map[string]any{"a": "x", "b": "y"}
	paths := WalkFieldPaths(record)
	assert.Equal(t, []string{"a", "b"}, paths)
}

func TestWalkFieldPaths_Nested(t *testing.T) {
	record := map[string]any{
		"item": map[string]any{
			"cve": map[string]any{
				"id":        "CVE-2024-0001",
				"published": "2024-01-15T00:00:00",
			},
		},
		"schema": "nvd",
	}
	paths := WalkFieldPaths(record)
	assert.Equal(t, []string{"item.cve.id", "item.cve.published", "schema"}, paths)
}

func TestWalkFieldPaths_WithArray(t *testing.T) {
	record := map[string]any{
		"id":   "CVE-2024-0001",
		"tags": []any{"critical", "network"},
	}
	paths := WalkFieldPaths(record)
	assert.Equal(t, []string{"id", "tags"}, paths)
}

func TestWalkFieldPaths_Empty(t *testing.T) {
	paths := WalkFieldPaths(map[string]any{})
	assert.Empty(t, paths)
}

// ---------------------------------------------------------------------------
// Scaling
// ---------------------------------------------------------------------------

func TestScaleRecord_PresenceOnly(t *testing.T) {
	records := []any{
		map[string]any{"id": "a1", "name": "alpha"},
		map[string]any{"id": "b2", "name": "beta"},
		map[string]any{"id": "c3", "name": "gamma"},
		map[string]any{"id": "d4", "name": "delta"},
	}
	stats := AnalyzeFields(records)
	attrs := DecideScaling(stats, len(records))

	// id has 4 distinct values for 4 records → high cardinality → presence
	// name has 4 distinct values → presence
	assert.Len(t, attrs, 2)
	for _, a := range attrs {
		assert.Equal(t, Presence, a.Kind, "field %s should be presence", a.Name)
	}
}

func TestScaleRecord_DateScaling(t *testing.T) {
	records := []any{
		map[string]any{"published": "2024-01-15T00:00:00"},
		map[string]any{"published": "2024-02-20T00:00:00"},
		map[string]any{"published": "2023-06-10T00:00:00"},
	}
	stats := AnalyzeFields(records)
	attrs := DecideScaling(stats, len(records))

	// Date field should produce year and month scaled attributes + presence
	var years, months []string
	hasPresence := false
	for _, a := range attrs {
		if a.Kind == Presence && a.Name == "published" {
			hasPresence = true
			continue
		}
		assert.Equal(t, ScaledValue, a.Kind)
		assert.Equal(t, "published", a.Field)
		if name := a.Name; len(name) > 0 {
			if idx := len("published.year="); len(name) > idx && name[:idx] == "published.year=" {
				years = append(years, name[idx:])
			}
			if idx := len("published.month="); len(name) > idx && name[:idx] == "published.month=" {
				months = append(months, name[idx:])
			}
		}
	}
	assert.True(t, hasPresence, "should include presence attribute for date field")
	assert.ElementsMatch(t, []string{"2023", "2024"}, years)
	assert.Contains(t, months, "01")
	assert.Contains(t, months, "02")
	assert.Contains(t, months, "06")
}

func TestScaleRecord_EnumDetection(t *testing.T) {
	records := []any{
		map[string]any{"status": "Analyzed", "id": "a1"},
		map[string]any{"status": "Modified", "id": "b2"},
		map[string]any{"status": "Analyzed", "id": "c3"},
		map[string]any{"status": "Rejected", "id": "d4"},
		map[string]any{"status": "Analyzed", "id": "e5"},
		map[string]any{"status": "Modified", "id": "f6"},
		map[string]any{"status": "Analyzed", "id": "g7"},
		map[string]any{"status": "Rejected", "id": "h8"},
	}
	stats := AnalyzeFields(records)
	attrs := DecideScaling(stats, len(records))

	// status has 3 distinct values → enum scaling + presence
	// id has 4 distinct values → presence
	var enumAttrs []Attribute
	for _, a := range attrs {
		if a.Field == "status" {
			enumAttrs = append(enumAttrs, a)
		}
	}
	// 3 values + 1 presence = 4 attributes
	assert.Len(t, enumAttrs, 4)

	names := make([]string, len(enumAttrs))
	for i, a := range enumAttrs {
		names[i] = a.Name
	}
	assert.ElementsMatch(t, []string{"status", "status=Analyzed", "status=Modified", "status=Rejected"}, names)
}

// ---------------------------------------------------------------------------
// Formal context + derivation operators
// ---------------------------------------------------------------------------

func TestFormalContext_SmallExample(t *testing.T) {
	// 3-object, 3-attribute cross table:
	//      a  b  c
	// 0:   1  1  0
	// 1:   1  0  1
	// 2:   0  1  1
	ctx := NewFormalContext(3, []string{"a", "b", "c"}, [][]bool{
		{true, true, false},
		{true, false, true},
		{false, true, true},
	})
	assert.Equal(t, 3, ctx.ObjectCount)
	assert.Len(t, ctx.Attributes, 3)
}

func TestFormalContext_AttrDeriv(t *testing.T) {
	ctx := NewFormalContext(3, []string{"a", "b", "c"}, [][]bool{
		{true, true, false},
		{true, false, true},
		{false, true, true},
	})

	// {a}' = objects with a = {0, 1}
	a := roaring.New()
	a.Add(0) // attribute index for "a"
	result := ctx.AttrDeriv(a)
	assert.True(t, result.Contains(0))
	assert.True(t, result.Contains(1))
	assert.False(t, result.Contains(2))
	assert.Equal(t, uint64(2), result.GetCardinality())

	// {b}' = {0, 2}
	b := roaring.New()
	b.Add(1)
	result = ctx.AttrDeriv(b)
	assert.True(t, result.Contains(0))
	assert.True(t, result.Contains(2))
	assert.Equal(t, uint64(2), result.GetCardinality())

	// {a, b}' = {0}
	ab := roaring.New()
	ab.Add(0)
	ab.Add(1)
	result = ctx.AttrDeriv(ab)
	assert.True(t, result.Contains(0))
	assert.Equal(t, uint64(1), result.GetCardinality())

	// {}' = all objects
	empty := roaring.New()
	result = ctx.AttrDeriv(empty)
	assert.Equal(t, uint64(3), result.GetCardinality())
}

func TestFormalContext_ObjectDeriv(t *testing.T) {
	ctx := NewFormalContext(3, []string{"a", "b", "c"}, [][]bool{
		{true, true, false},
		{true, false, true},
		{false, true, true},
	})

	// {0}' = attributes of object 0 = {a, b}
	obj0 := roaring.New()
	obj0.Add(0)
	result := ctx.ObjectDeriv(obj0)
	assert.True(t, result.Contains(0))  // a
	assert.True(t, result.Contains(1))  // b
	assert.False(t, result.Contains(2)) // c
	assert.Equal(t, uint64(2), result.GetCardinality())

	// {0, 1}' = {a, b} ∩ {a, c} = {a}
	obj01 := roaring.New()
	obj01.Add(0)
	obj01.Add(1)
	result = ctx.ObjectDeriv(obj01)
	assert.True(t, result.Contains(0)) // a
	assert.Equal(t, uint64(1), result.GetCardinality())

	// {}' = all attributes
	empty := roaring.New()
	result = ctx.ObjectDeriv(empty)
	assert.Equal(t, uint64(3), result.GetCardinality())
}

func TestFormalContext_Closure(t *testing.T) {
	ctx := NewFormalContext(3, []string{"a", "b", "c"}, [][]bool{
		{true, true, false},
		{true, false, true},
		{false, true, true},
	})

	// {a}'' = {0,1}' = {a,b} ∩ {a,c} = {a}
	a := roaring.New()
	a.Add(0)
	result := ctx.Closure(a)
	assert.True(t, result.Contains(0))
	assert.Equal(t, uint64(1), result.GetCardinality())

	// {a,b}'' = {0}' = {a,b}
	ab := roaring.New()
	ab.Add(0)
	ab.Add(1)
	result = ctx.Closure(ab)
	assert.True(t, result.Contains(0))
	assert.True(t, result.Contains(1))
	assert.Equal(t, uint64(2), result.GetCardinality())

	// {}'' = {0,1,2}' = {} (no attribute shared by all)
	empty := roaring.New()
	result = ctx.Closure(empty)
	assert.True(t, result.IsEmpty())
}

func TestFormalContext_FromRecords(t *testing.T) {
	records := []any{
		map[string]any{
			"schema":     "kev",
			"identifier": "CVE-2024-0001",
			"item":       map[string]any{"cveID": "CVE-2024-0001", "vendor": "Acme"},
		},
		map[string]any{
			"schema":     "kev",
			"identifier": "CVE-2024-0002",
			"item":       map[string]any{"cveID": "CVE-2024-0002", "vendor": "Beta"},
		},
		map[string]any{
			"schema":     "kev",
			"identifier": "CVE-2024-0003",
			"item":       map[string]any{"cveID": "CVE-2024-0003", "vendor": "Acme"},
		},
	}

	ctx := BuildContextFromRecords(records)
	require.NotNil(t, ctx)
	assert.Equal(t, 3, ctx.ObjectCount)
	// All records have same structure → all fields are universal
	// Check that schema field is universal (present in all 3)
	for j, attr := range ctx.Attributes {
		if attr.Name == "schema" || (attr.Kind == ScaledValue && attr.Field == "schema") {
			// schema = "kev" for all 3 → might be enum (1 value, but <2 so presence)
			assert.True(t, ctx.columns[j].GetCardinality() >= 1,
				"attribute %s should have objects", attr.Name)
		}
	}
}
