package lattice

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/agentic-research/mache/api"
)

// InferGreedy performs schema inference using a greedy entropy-based partitioning algorithm.
func InferGreedy(records []any, config ProjectConfig) *api.Topology {
	// Root depth is 0
	root := buildTreeRecursive(records, config.RootName, 0, config.MaxDepth, config.Hints)
	// Root selector must be "$" to pass the list to children filters
	root.Selector = "$"
	return &api.Topology{
		Version: "v1",
		Nodes:   []api.Node{root},
	}
}

func buildTreeRecursive(records []any, name string, depth, maxDepth int, hints map[string]string) api.Node {
	// Base cases
	if depth >= maxDepth || len(records) < 10 {
		return makeLeafNode(name, records, hints)
	}

	// 1. Analyze fields
	stats := AnalyzeFields(records)

	// 2. Identify candidate fields for splitting
	var candidates []string

	processedFields := make(map[string]bool)

	for field, fs := range stats {
		hint := hints[field]

		// If hint says "id" or "reference", skip immediately
		if hint == "id" || hint == "reference" {
			continue
		}

		// If hint says "temporal", add virtual candidates
		if hint == "temporal" || (hint == "" && fs.IsDate && fs.Cardinality > 10) {
			// Add .year, .month, .day candidates
			candidates = append(candidates, field+":year")
			candidates = append(candidates, field+":month")
			candidates = append(candidates, field+":day")

			// Also add raw field only if cardinality is low enough to be a directory
			if fs.Cardinality < 50 {
				candidates = append(candidates, field)
			}
			processedFields[field] = true
			continue
		}

		// Standard handling
		if fs.Cardinality < 2 {
			// Sparse field check
			if fs.Count == len(records) {
				continue
			}
		}

		// Skip likely IDs (heuristic), unless overridden by hint
		if hint == "" {
			ratio := float64(fs.Cardinality) / float64(len(records))
			if len(records) > 10 && ratio > 0.9 {
				continue
			}
		}

		candidates = append(candidates, field)
		processedFields[field] = true
	}

	sort.Strings(candidates)

	// 3. If no candidates, stop
	if len(candidates) == 0 {
		return makeLeafNode(name, records, hints)
	}

	// 4. Find best attribute (field or virtual field)
	bestAttr, bestScore := selectBestAttribute(records, candidates, stats, hints)

	// Threshold
	if bestScore < 0.5 {
		return makeLeafNode(name, records, hints)
	}
	// 5. Partition records
	partitions := partitionByAttribute(records, bestAttr)

	// 6. Build children
	children := make([]api.Node, 0, len(partitions))

	keys := make([]string, 0, len(partitions))
	for k := range partitions {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	field, modifier := parseAttribute(bestAttr)

	for _, key := range keys {
		subset := partitions[key]
		childName := sanitizeName(key)

		// Selector generation
		var childSelector string
		escKey := escapeSelectorValue(key)

		if key == "__MACHE_MISSING__" {
			childSelector = fmt.Sprintf("$[?(!(@.%s))]", field)
		} else {
			switch modifier {
			case "year":
				childSelector = fmt.Sprintf("$[?(@.%s =~ '^%s')]", field, escKey)
			case "month":
				// Match YYYY-MM
				childSelector = fmt.Sprintf("$[?(@.%s =~ '^.{5}%s')]", field, escKey)
			case "day":
				// Match YYYY-MM-DD
				childSelector = fmt.Sprintf("$[?(@.%s =~ '^.{8}%s')]", field, escKey)
			default:
				childSelector = fmt.Sprintf("$[?(@.%s == '%s')]", field, escKey)
			}
		}

		subNode := buildTreeRecursive(subset, childName, depth+1, maxDepth, hints)
		subNode.Selector = childSelector
		children = append(children, subNode)
	}

	return api.Node{
		Name:     name,
		Selector: "$[*]",
		Children: children,
	}
}

func makeLeafNode(name string, records []any, hints map[string]string) api.Node {
	ctx := BuildContextFromRecords(records)

	// Collect universal fields manually from stats

	var universal []universalFieldInfo

	for field, fs := range ctx.Stats {
		if fs.Count == len(records) {
			universal = append(universal, universalFieldInfo{
				path: field,

				cardinality: fs.Cardinality,
			})
		}
	}

	// Detect identifier

	// Prefer field hinted as "id"

	var idField string

	for _, f := range universal {
		if hints[f.path] == "id" {

			idField = f.path

			break

		}
	}

	if idField == "" {
		idField = detectIdentifier(universal, ctx)
	}

	if idField == "" {
		idField = "id" // Fallback
	}

	// Collect other fields for files

	var leafFields []string

	for _, f := range universal {
		if f.path != idField {
			leafFields = append(leafFields, f.path)
		}
	}

	sort.Strings(leafFields)

	leaves := greedyBuildLeafFiles(leafFields)

	leaves = append(leaves, api.Leaf{
		Name: "raw.json",

		ContentTemplate: "{{. | json}}",
	})

	// Iterator node (creates directory per record)

	iterator := api.Node{
		Name: fmt.Sprintf("{{.%s}}", idField),

		Selector: "$[*]",

		Files: leaves,
	}

	return api.Node{
		Name: name,

		Selector: "$", // Will be overwritten by caller if not root

		Children: []api.Node{iterator},
	}
}

// selectBestAttribute selects the attribute that maximizes a weighted score.

// Score = StructuralGain + (IntrinsicEntropy * 0.1) + HintBoost

func selectBestAttribute(records []any, candidates []string, stats map[string]*FieldStats, hints map[string]string) (string, float64) {
	signatures := make([]string, len(records))

	for i, rec := range records {
		signatures[i] = getSchemaSignature(rec)
	}

	baseEntropy := calculateEntropyFromSignatures(signatures)

	bestAttr := ""

	bestScore := -1.0

	bestCard := math.MaxInt32

	bestCount := -1

	for _, attr := range candidates {

		partitions := partitionByAttribute(records, attr)

		distinctValues := len(partitions)

		// 1. Calculate Structural Gain

		weightedEntropy := 0.0

		total := float64(len(records))

		for _, subset := range partitions {

			subSigs := make([]string, len(subset))

			for k, rec := range subset {
				subSigs[k] = getSchemaSignature(rec)
			}

			weight := float64(len(subset)) / total

			weightedEntropy += weight * calculateEntropyFromSignatures(subSigs)

		}

		structuralGain := baseEntropy - weightedEntropy

		// 2. Calculate Intrinsic Entropy (Distribution)

		intrinsicEntropy := 0.0

		for _, subset := range partitions {

			p := float64(len(subset)) / total

			if p > 0 {
				intrinsicEntropy -= p * math.Log2(p)
			}

		}

		// 3. Compute Score

		// Start with structural gain

		score := structuralGain

		// If structural gain is negligible, use intrinsic entropy to encourage partitioning large buckets

		// But scale it down so it doesn't override structural differences

		if structuralGain < 0.001 {
			score += intrinsicEntropy * 0.1
		}

		// 4. Apply Hint Boost

		field, mod := parseAttribute(attr)

		hint := hints[field]

		if hint == "temporal" {
			// Boost temporal fields if they actually split data

			if intrinsicEntropy > 0.01 {

				score += 10.0

				if mod == "year" {
					score += 3.0
				}

				if mod == "month" {
					score += 2.0
				}

			}
		}

		// Tie-Breaking
		fieldRaw, _ := parseAttribute(attr)
		currentCount := 0
		if fs, ok := stats[fieldRaw]; ok {
			currentCount = fs.Count
		}

		isBetter := false
		if score > bestScore+0.000001 {
			isBetter = true
		} else if math.Abs(score-bestScore) < 0.000001 {
			// Tie on score
			if currentCount > bestCount {
				isBetter = true
			} else if currentCount == bestCount {
				// Tie on support
				if distinctValues < bestCard {
					isBetter = true
				}
			}
		}

		if isBetter {
			bestAttr = attr
			bestScore = score
			bestCard = distinctValues
			bestCount = currentCount
		}
	}

	return bestAttr, bestScore
}

func parseAttribute(attr string) (string, string) {
	if strings.Contains(attr, ":") {
		parts := strings.Split(attr, ":")
		return parts[0], parts[1]
	}
	return attr, ""
}

func partitionByAttribute(records []any, attr string) map[string][]any {
	field, modifier := parseAttribute(attr)
	partitions := make(map[string][]any)

	for _, rec := range records {
		val, ok := getFieldValue(rec, field)
		key := "__MACHE_MISSING__"

		if ok {
			s := fmt.Sprintf("%v", val)
			switch modifier {
			case "year":
				if len(s) >= 4 {
					key = s[:4]
				} else {
					key = "invalid_date"
				}
			case "month":
				if len(s) >= 7 {
					key = s[5:7] // YYYY-MM-DD -> 01
				} else {
					key = "invalid_date"
				}
			case "day":
				if len(s) >= 10 {
					key = s[8:10]
				} else {
					key = "invalid_date"
				}
			default:
				key = s
			}
		}
		partitions[key] = append(partitions[key], rec)
	}
	return partitions
}

func getSchemaSignature(rec any) string {
	paths := WalkFieldPaths(rec)
	return strings.Join(paths, "|")
}

func calculateEntropyFromSignatures(signatures []string) float64 {
	if len(signatures) == 0 {
		return 0
	}
	counts := make(map[string]int)
	for _, s := range signatures {
		counts[s]++
	}

	entropy := 0.0
	total := float64(len(signatures))
	for _, c := range counts {
		p := float64(c) / total
		if p > 0 {
			entropy -= p * math.Log2(p)
		}
	}
	return entropy
}

func sanitizeName(s string) string {
	safe := strings.ReplaceAll(s, "/", "-")
	safe = strings.ReplaceAll(safe, "\\", "-")
	safe = strings.ReplaceAll(safe, ":", "-")
	safe = strings.ReplaceAll(safe, "*", "-")
	safe = strings.ReplaceAll(safe, "?", "-")
	safe = strings.ReplaceAll(safe, "\"", "-")
	safe = strings.ReplaceAll(safe, "<", "-")
	safe = strings.ReplaceAll(safe, ">", "-")
	safe = strings.ReplaceAll(safe, "|", "-")
	return safe
}

func escapeSelectorValue(s string) string {
	return strings.ReplaceAll(s, "'", "\\'")
}

func greedyBuildLeafFiles(fields []string) []api.Leaf {
	var leaves []api.Leaf
	for _, f := range fields {
		parts := strings.Split(f, ".")
		name := parts[len(parts)-1]
		name = greedyCamelToKebab(name)

		leaves = append(leaves, api.Leaf{
			Name:            name,
			ContentTemplate: fmt.Sprintf("{{.%s}}", f),
		})
	}
	return leaves
}

func greedyCamelToKebab(s string) string {
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
