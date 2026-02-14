package lattice

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGreedyInference(t *testing.T) {
	// Create synthetic data
	records := []any{
		map[string]any{"type": "vuln", "year": "2021", "id": "CVE-2021-001"},
		map[string]any{"type": "vuln", "year": "2021", "id": "CVE-2021-002"},
		map[string]any{"type": "vuln", "year": "2022", "id": "CVE-2022-001"},
		map[string]any{"type": "patch", "year": "2021", "id": "KB001"},
		map[string]any{"type": "patch", "year": "2022", "id": "KB002"},
		// Add more to trigger split logic (need > 10 records)
		map[string]any{"type": "vuln", "year": "2021", "id": "CVE-2021-003"},
		map[string]any{"type": "vuln", "year": "2021", "id": "CVE-2021-004"},
		map[string]any{"type": "vuln", "year": "2022", "id": "CVE-2022-002"},
		map[string]any{"type": "patch", "year": "2021", "id": "KB003"},
		map[string]any{"type": "patch", "year": "2022", "id": "KB004"},
		map[string]any{"type": "vuln", "year": "2023", "id": "CVE-2023-001"},
	}

	config := DefaultProjectConfig()
	config.MaxDepth = 3

	topology := InferGreedy(records, config)

	assert.Equal(t, "v1", topology.Version)
	assert.Len(t, topology.Nodes, 1)
	root := topology.Nodes[0]
	assert.Equal(t, "records", root.Name)

	// Should have children split by 'type' or 'year'
	// 'type' has 2 values (vuln, patch). 'year' has 3 (2021, 2022, 2023).
	// 'id' has 11 values (unique).

	// Entropy of 'type': 7 vuln, 4 patch. p=7/11, q=4/11. H ~ 0.94
	// Entropy of 'year': 5 2021, 5 2022, 1 2023. H ~ 1.3

	// 'type' likely splits better because it separates structure?
	// Here structure is identical (all have id, type, year).
	// So Schema Entropy is 0.
	// Gain for all attributes will be 0 based on Schema Signature.

	// If schema is identical, Greedy should stop and return a flat list.
	// But let's verify if it produces ANY children.
	// If it produces children, it means it found a differentiator.

	// Wait, if schema is identical, my implementation returns a leaf node!
	// This is correct behavior for schema inference (no structural difference = same schema).

	// Let's introduce structural difference.
	records = append(records,
		map[string]any{"type": "advisory", "severity": "high", "id": "ADV-001"},
		map[string]any{"type": "advisory", "severity": "low", "id": "ADV-002"},
	)

	// Now 'type'="advisory" has 'severity', others don't.
	// 'type' splits perfectly into {vuln, patch} (no severity) and {advisory} (has severity).
	// So 'type' should be the chosen attribute.

	topology = InferGreedy(records, config)
	root = topology.Nodes[0]

	assert.NotEmpty(t, root.Children, "Should have split on type")

	// Verify split on 'type'
	// One child should correspond to 'advisory'
	foundAdvisory := false
	for _, child := range root.Children {
		// Child name is the value
		if child.Name == "advisory" {
			foundAdvisory = true
			assert.Contains(t, child.Selector, "@.type == 'advisory'")
		}
	}
	assert.True(t, foundAdvisory, "Should find 'advisory' partition")
}

func TestIntegration(t *testing.T) {
	inf := Inferrer{
		Config: InferConfig{
			Method:   "greedy",
			MaxDepth: 3,
		},
	}

	records := []any{
		map[string]any{"a": 1},
		map[string]any{"b": 2}, // Different schema
	}
	// Need > 10 records for split?
	// My implementation checks `len(records) < 10` in `buildTreeRecursive`.
	// So with 2 records it returns leaf.

	// Let's override for test or provide more records.
	for i := 0; i < 5; i++ {
		records = append(records, map[string]any{"a": 1})
		records = append(records, map[string]any{"b": 2})
	}

	topo, err := inf.InferFromRecords(records)
	assert.NoError(t, err)
	assert.NotEmpty(t, topo.Nodes[0].Children)
}

func TestGreedyInference_WithHints(t *testing.T) {
	records := []any{
		map[string]any{"sha": "abc1", "ts": "2024-01-01", "msg": "fix"},
		map[string]any{"sha": "abc2", "ts": "2024-01-02", "msg": "feat"},
		map[string]any{"sha": "abc3", "ts": "2024-02-01", "msg": "chore"},
		map[string]any{"sha": "abc4", "ts": "2023-12-31", "msg": "init"},
	}
	// Duplicate records to exceed min threshold (10)
	baseLen := len(records)
	for i := 0; i < 3; i++ {
		for j := 0; j < baseLen; j++ {
			// Deep copy map to avoid reference issues if modified (though strictly not needed here)
			orig := records[j].(map[string]any)
			dup := make(map[string]any)
			for k, v := range orig {
				dup[k] = v
			}
			records = append(records, dup)
		}
	}

	config := DefaultProjectConfig()
	config.MaxDepth = 5
	config.Hints = map[string]string{
		"sha": "id",
		"ts":  "temporal",
	}

	topology := InferGreedy(records, config)
	root := topology.Nodes[0]

	// Should NOT split by sha (flat list of 4 children? No, sha is excluded).
	// Should split by ts:year.
	// Years: 2024 (3 recs), 2023 (1 rec).

	// Check children
	found2024 := false
	found2023 := false

	for _, child := range root.Children {
		if child.Name == "2024" {
			found2024 = true
			// Inside 2024, should split by month (01, 02)
			assert.NotEmpty(t, child.Children, "2024 should have month children")
			for _, grandChild := range child.Children {
				if grandChild.Name == "01" {
					assert.Contains(t, grandChild.Selector, "ts =~ '^.{5}01'")
				}
			}
		}
		if child.Name == "2023" {
			found2023 = true
		}
	}

	var childNames []string
	for _, child := range root.Children {
		childNames = append(childNames, child.Name)
	}

	assert.True(t, found2024, "Should find 2024 partition. Found: %v", childNames)
	assert.True(t, found2023, "Should find 2023 partition")

	// Verify SHA is NOT used as a directory
	// If SHA was used, we would see "abc1" etc as children of root (since it has high entropy).
	// But TS also has high entropy?
	// TS raw values are unique. SHA raw values are unique.
	// Without hints, both are candidates.
	// With hints, SHA is excluded. TS is virtualized.
}
