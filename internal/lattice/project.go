package lattice

import (
	"fmt"
	"sort"
	"strings"

	"github.com/RoaringBitmap/roaring"
	"github.com/agentic-research/mache/api"
)

// ProjectConfig controls how the lattice is projected into a topology.
type ProjectConfig struct {
	RootName string            // directory name for the root node (default: "records")
	MaxDepth int               // maximum depth for recursive inference (default: 5)
	Hints    map[string]string // hints for attribute types ("id", "temporal", "reference")
}

// DefaultProjectConfig returns sensible defaults.
func DefaultProjectConfig() ProjectConfig {
	return ProjectConfig{
		RootName: "records",
		MaxDepth: 5,
		Hints:    make(map[string]string),
	}
}

// Project walks the concept lattice and emits an api.Topology.
//
// Projection rules:
//  1. Universal attributes (top concept intent) identify fields present in ALL records.
//  2. Identifier field = highest-cardinality universal string field → directory name template.
//  3. Shard levels = date-scaled attributes with 2-100 distinct groups → directory levels.
//  4. Leaf files = remaining universal scalar fields → file content templates.
//  5. raw.json is always included.
func Project(concepts []Concept, ctx *FormalContext, config ProjectConfig) *api.Topology {
	if len(concepts) == 0 {
		return &api.Topology{Version: "v1"}
	}
	if config.RootName == "" {
		config.RootName = "records"
	}

	// 1. Find top concept (largest extent = all objects)
	top := concepts[0]
	for _, c := range concepts[1:] {
		if c.Extent.GetCardinality() > top.Extent.GetCardinality() {
			top = c
		}
	}

	// 2. Collect universal presence fields and date shard sources
	universalFields := collectUniversalFields(top.Intent, ctx)
	dateFields := collectDateFields(ctx)
	identifier := detectIdentifier(universalFields, ctx)
	shardLevels := detectShardLevels(dateFields, ctx)
	leafFields := selectLeafFields(universalFields, identifier, shardLevels)

	// 3. Build topology tree
	return buildTopology(config, identifier, shardLevels, leafFields)
}

// universalFieldInfo holds info about a field that's present in all records.
type universalFieldInfo struct {
	path        string
	cardinality int
}

func collectUniversalFields(topIntent *roaring.Bitmap, ctx *FormalContext) []universalFieldInfo {
	seen := make(map[string]bool)
	var fields []universalFieldInfo

	iter := topIntent.Iterator()
	for iter.HasNext() {
		j := iter.Next()
		attr := ctx.Attributes[j]
		fieldPath := attr.Field
		if seen[fieldPath] {
			continue
		}
		seen[fieldPath] = true

		card := 0
		if ctx.Stats != nil {
			if fs, ok := ctx.Stats[fieldPath]; ok {
				card = fs.Cardinality
			}
		}
		fields = append(fields, universalFieldInfo{path: fieldPath, cardinality: card})
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].path < fields[j].path })
	return fields
}

// collectDateFields returns field paths that have date scaling.
func collectDateFields(ctx *FormalContext) map[string]bool {
	dates := make(map[string]bool)
	for _, attr := range ctx.Attributes {
		if attr.Kind == ScaledValue && strings.Contains(attr.Name, ".year=") {
			dates[attr.Field] = true
		}
	}
	return dates
}

// detectIdentifier finds the highest-cardinality universal string field.
func detectIdentifier(fields []universalFieldInfo, ctx *FormalContext) string {
	best := ""
	bestCard := 0
	dateFields := collectDateFields(ctx)

	for _, f := range fields {
		if dateFields[f.path] {
			continue
		}
		if f.cardinality > bestCard {
			bestCard = f.cardinality
			best = f.path
		}
	}
	return best
}

// shardLevel represents a directory level from date scaling.
type shardLevel struct {
	field     string // source field path
	component string // "year" or "month"
	start     int    // slice start index into the date string
	end       int    // slice end index
}

func detectShardLevels(dateFields map[string]bool, ctx *FormalContext) []shardLevel {
	// Pick the single best date field for sharding (most distinct years).
	// Using multiple date fields creates over-deep directory trees.
	var bestField string
	bestYears := 0

	for field := range dateFields {
		yearCount := 0
		for _, attr := range ctx.Attributes {
			if attr.Field == field && attr.Kind == ScaledValue && strings.Contains(attr.Name, ".year=") {
				yearCount++
			}
		}
		if yearCount > bestYears {
			bestYears = yearCount
			bestField = field
		}
	}

	if bestField == "" || bestYears < 2 {
		return nil
	}

	var levels []shardLevel
	levels = append(levels, shardLevel{
		field: bestField, component: "year", start: 0, end: 4,
	})

	monthCount := 0
	for _, attr := range ctx.Attributes {
		if attr.Field == bestField && attr.Kind == ScaledValue && strings.Contains(attr.Name, ".month=") {
			monthCount++
		}
	}
	if monthCount >= 2 {
		levels = append(levels, shardLevel{
			field: bestField, component: "month", start: 5, end: 7,
		})
	}

	return levels
}

func selectLeafFields(fields []universalFieldInfo, identifier string, shards []shardLevel) []string {
	shardFields := make(map[string]bool)
	for _, s := range shards {
		shardFields[s.field] = true
	}

	var leaves []string
	for _, f := range fields {
		if f.path == identifier || shardFields[f.path] {
			continue
		}
		leaves = append(leaves, f.path)
	}
	return leaves
}

func buildTopology(config ProjectConfig, identifier string, shards []shardLevel, leafFields []string) *api.Topology {
	leaves := buildLeafFiles(leafFields)

	// Always include raw.json
	leaves = append(leaves, api.Leaf{
		Name:            "raw.json",
		ContentTemplate: "{{. | json}}",
	})

	// Build identifier node (innermost directory)
	idTemplate := "{{.identifier}}"
	if identifier != "" {
		idTemplate = fmt.Sprintf("{{.%s}}", identifier)
	}
	innermost := api.Node{
		Name:     idTemplate,
		Selector: "$",
		Files:    leaves,
	}

	// Wrap in shard levels (innermost first → outermost last)
	current := innermost
	for i := len(shards) - 1; i >= 0; i-- {
		s := shards[i]
		tmpl := fmt.Sprintf("{{slice .%s %d %d}}", s.field, s.start, s.end)
		wrapper := api.Node{
			Name:     tmpl,
			Selector: "$",
			Children: []api.Node{current},
		}
		current = wrapper
	}

	// The outermost shard (or identifier if no shards) gets the $[*] selector
	// to iterate over records
	current.Selector = "$[*]"

	root := api.Node{
		Name:     config.RootName,
		Selector: "$",
		Children: []api.Node{current},
	}

	return &api.Topology{
		Version: "v1",
		Nodes:   []api.Node{root},
	}
}

func buildLeafFiles(fields []string) []api.Leaf {
	var leaves []api.Leaf
	for _, f := range fields {
		// Use last path component as file name, with dots replaced by hyphens
		parts := strings.Split(f, ".")
		name := parts[len(parts)-1]
		// Convert camelCase to kebab-case
		name = camelToKebab(name)

		leaves = append(leaves, api.Leaf{
			Name:            name,
			ContentTemplate: fmt.Sprintf("{{.%s}}", f),
		})
	}
	return leaves
}

// camelToKebab converts camelCase to kebab-case.
// Handles acronyms: "vendorProject" → "vendor-project", "cveID" → "cve-id"
func camelToKebab(s string) string {
	runes := []rune(s)
	var result []rune
	for i, r := range runes {
		if i > 0 && r >= 'A' && r <= 'Z' {
			prev := runes[i-1]
			if prev >= 'a' && prev <= 'z' {
				result = append(result, '-')
			}
		}
		if r >= 'A' && r <= 'Z' {
			result = append(result, r+32) // lowercase
		} else {
			result = append(result, r)
		}
	}
	return string(result)
}
