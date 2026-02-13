package lattice

import (
	"regexp"
	"sort"
	"strings"

	"github.com/RoaringBitmap/roaring"
)

// AttributeKind classifies how a JSON field is converted to binary attributes.
type AttributeKind int

const (
	// Presence means the field path exists in the record.
	Presence AttributeKind = iota
	// ScaledValue means the attribute represents a specific value (e.g., year=2024).
	ScaledValue
)

// Attribute is a named binary property in the formal context.
type Attribute struct {
	Name  string        // e.g., "item.cve.id" or "item.published.year=2024"
	Kind  AttributeKind // Presence or ScaledValue
	Field string        // original field path (for ScaledValue, the source field)
}

// FieldStats holds statistics about a single field across all sampled records.
type FieldStats struct {
	Count       int            // how many records have this field
	Cardinality int            // number of distinct values
	IsDate      bool           // whether values match ISO date pattern
	Values      map[string]int // distinct value → count
}

// FormalContext is a bitmap-based incidence table for Formal Concept Analysis.
// Column-major storage: each attribute has a bitmap of which objects possess it.
type FormalContext struct {
	ObjectCount int
	Attributes  []Attribute
	columns     []*roaring.Bitmap // columns[j] = objects with attribute j
	rows        []*roaring.Bitmap // rows[i] = attributes of object i (lazy)
	attrIndex   map[string]int    // attribute name → index
	Stats       map[string]*FieldStats
}

var dateRe = regexp.MustCompile(`^\d{4}-\d{2}`)

const (
	enumMaxDistinct      = 20
	identifierRatioThres = 0.5
)

// WalkFieldPaths extracts all leaf field paths from a JSON-like value.
// Returns sorted, unique paths using dot notation (e.g., "item.cve.id").
func WalkFieldPaths(v any) []string {
	var paths []string
	walkPaths(v, "", &paths)
	sort.Strings(paths)
	return paths
}

func walkPaths(v any, prefix string, paths *[]string) {
	switch val := v.(type) {
	case map[string]any:
		for k, child := range val {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			walkPaths(child, p, paths)
		}
	default:
		// Scalar or array — record the path
		if prefix != "" {
			*paths = append(*paths, prefix)
		}
	}
}

// AnalyzeFields examines all sampled records to gather field statistics.
func AnalyzeFields(records []any) map[string]*FieldStats {
	stats := make(map[string]*FieldStats)
	for _, rec := range records {
		paths := WalkFieldPaths(rec)
		for _, path := range paths {
			fs, ok := stats[path]
			if !ok {
				fs = &FieldStats{Values: make(map[string]int)}
				stats[path] = fs
			}
			fs.Count++
			if val, ok := getFieldValue(rec, path); ok {
				if s, ok := val.(string); ok {
					fs.Values[s]++
					if dateRe.MatchString(s) {
						fs.IsDate = true
					}
				}
			}
		}
	}
	for _, fs := range stats {
		fs.Cardinality = len(fs.Values)
	}
	return stats
}

// DecideScaling determines which attributes to create from field statistics.
func DecideScaling(stats map[string]*FieldStats, totalRecords int) []Attribute {
	// Collect field paths and sort for deterministic attribute ordering
	var fields []string
	for f := range stats {
		fields = append(fields, f)
	}
	sort.Strings(fields)

	var attrs []Attribute
	for _, field := range fields {
		// Always add Presence attribute so the field is known to exist
		attrs = append(attrs, Attribute{
			Name:  field,
			Kind:  Presence,
			Field: field,
		})

		fs := stats[field]
		switch {
		case fs.IsDate && fs.Count > totalRecords/2:
			// Date scaling: add year and month value attributes
			years := make(map[string]bool)
			months := make(map[string]bool)
			for val := range fs.Values {
				if len(val) >= 7 {
					years[val[:4]] = true
					months[val[5:7]] = true
				}
			}
			sortedYears := sortedKeys(years)
			for _, y := range sortedYears {
				attrs = append(attrs, Attribute{
					Name:  field + ".year=" + y,
					Kind:  ScaledValue,
					Field: field,
				})
			}
			sortedMonths := sortedKeys(months)
			for _, m := range sortedMonths {
				attrs = append(attrs, Attribute{
					Name:  field + ".month=" + m,
					Kind:  ScaledValue,
					Field: field,
				})
			}
		case fs.Cardinality <= enumMaxDistinct && fs.Cardinality >= 2 &&
			float64(fs.Cardinality)/float64(fs.Count) <= identifierRatioThres:
			// Enum scaling: one attribute per distinct value
			sortedVals := sortedKeys(boolMap(fs.Values))
			for _, val := range sortedVals {
				attrs = append(attrs, Attribute{
					Name:  field + "=" + val,
					Kind:  ScaledValue,
					Field: field,
				})
			}
		}
	}
	return attrs
}

// BuildContext constructs a FormalContext from records using the given attributes.
func BuildContext(records []any, attrs []Attribute) *FormalContext {
	ctx := &FormalContext{
		ObjectCount: len(records),
		Attributes:  attrs,
		columns:     make([]*roaring.Bitmap, len(attrs)),
		attrIndex:   make(map[string]int, len(attrs)),
	}
	for j := range attrs {
		ctx.columns[j] = roaring.New()
		ctx.attrIndex[attrs[j].Name] = j
	}

	for i, rec := range records {
		for j, attr := range attrs {
			if hasAttribute(rec, attr) {
				ctx.columns[j].Add(uint32(i))
			}
		}
	}
	return ctx
}

// BuildContextFromRecords is a convenience that analyzes fields and builds a context.
func BuildContextFromRecords(records []any) *FormalContext {
	stats := AnalyzeFields(records)
	attrs := DecideScaling(stats, len(records))
	ctx := BuildContext(records, attrs)
	ctx.Stats = stats
	return ctx
}

// NewFormalContext creates a FormalContext from a pre-built incidence table.
// Used for unit tests with known cross-tables.
func NewFormalContext(objectCount int, attrNames []string, incidence [][]bool) *FormalContext {
	attrs := make([]Attribute, len(attrNames))
	for i, name := range attrNames {
		attrs[i] = Attribute{Name: name, Kind: Presence, Field: name}
	}
	ctx := &FormalContext{
		ObjectCount: objectCount,
		Attributes:  attrs,
		columns:     make([]*roaring.Bitmap, len(attrs)),
		attrIndex:   make(map[string]int, len(attrs)),
	}
	for j := range attrs {
		ctx.columns[j] = roaring.New()
		ctx.attrIndex[attrs[j].Name] = j
	}
	for i, row := range incidence {
		for j, has := range row {
			if has {
				ctx.columns[j].Add(uint32(i))
			}
		}
	}
	return ctx
}

// AttrDeriv computes B' — the set of objects that have ALL attributes in B.
func (ctx *FormalContext) AttrDeriv(attrs *roaring.Bitmap) *roaring.Bitmap {
	if attrs.IsEmpty() {
		result := roaring.New()
		result.AddRange(0, uint64(ctx.ObjectCount))
		return result
	}
	var result *roaring.Bitmap
	iter := attrs.Iterator()
	for iter.HasNext() {
		j := iter.Next()
		if int(j) >= len(ctx.columns) {
			return roaring.New()
		}
		if result == nil {
			result = ctx.columns[j].Clone()
		} else {
			result.And(ctx.columns[j])
		}
	}
	if result == nil {
		return roaring.New()
	}
	return result
}

// ObjectDeriv computes A' — the set of attributes common to ALL objects in A.
func (ctx *FormalContext) ObjectDeriv(objs *roaring.Bitmap) *roaring.Bitmap {
	if objs.IsEmpty() {
		result := roaring.New()
		for j := range ctx.Attributes {
			result.Add(uint32(j))
		}
		return result
	}
	ctx.ensureRows()
	var result *roaring.Bitmap
	iter := objs.Iterator()
	for iter.HasNext() {
		i := iter.Next()
		if int(i) >= len(ctx.rows) {
			return roaring.New()
		}
		if result == nil {
			result = ctx.rows[i].Clone()
		} else {
			result.And(ctx.rows[i])
		}
	}
	if result == nil {
		return roaring.New()
	}
	return result
}

// Closure computes B” = (B')'.
func (ctx *FormalContext) Closure(attrs *roaring.Bitmap) *roaring.Bitmap {
	return ctx.ObjectDeriv(ctx.AttrDeriv(attrs))
}

// ensureRows lazily computes row bitmaps from column bitmaps.
func (ctx *FormalContext) ensureRows() {
	if ctx.rows != nil {
		return
	}
	ctx.rows = make([]*roaring.Bitmap, ctx.ObjectCount)
	for i := range ctx.rows {
		ctx.rows[i] = roaring.New()
	}
	for j, col := range ctx.columns {
		iter := col.Iterator()
		for iter.HasNext() {
			i := iter.Next()
			ctx.rows[i].Add(uint32(j))
		}
	}
}

// hasAttribute checks whether a record possesses a given attribute.
func hasAttribute(rec any, attr Attribute) bool {
	switch attr.Kind {
	case Presence:
		_, ok := getFieldValue(rec, attr.Field)
		return ok
	case ScaledValue:
		// Parse "field.year=2024" or "field=value"
		val, ok := getFieldValue(rec, attr.Field)
		if !ok {
			return false
		}
		s, ok := val.(string)
		if !ok {
			return false
		}
		// Extract the expected suffix from attr.Name beyond the field
		suffix := strings.TrimPrefix(attr.Name, attr.Field)
		if strings.HasPrefix(suffix, ".year=") {
			year := strings.TrimPrefix(suffix, ".year=")
			return len(s) >= 4 && s[:4] == year
		}
		if strings.HasPrefix(suffix, ".month=") {
			month := strings.TrimPrefix(suffix, ".month=")
			return len(s) >= 7 && s[5:7] == month
		}
		// Enum: "field=value"
		if strings.HasPrefix(suffix, "=") {
			expected := strings.TrimPrefix(suffix, "=")
			return s == expected
		}
		return false
	}
	return false
}

// getFieldValue extracts a value from a nested map using dot-separated path.
func getFieldValue(v any, path string) (any, bool) {
	parts := strings.Split(path, ".")
	current := v
	for _, part := range parts {
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func boolMap(m map[string]int) map[string]bool {
	result := make(map[string]bool, len(m))
	for k := range m {
		result[k] = true
	}
	return result
}
