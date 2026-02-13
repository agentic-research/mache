package lattice

import (
	"encoding/json"
	"fmt"
	"math/rand"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/ingest"
	sitter "github.com/smacker/go-tree-sitter"
)

// InferConfig controls the schema inference pipeline.
type InferConfig struct {
	SampleSize int    // max records to sample (default 1000)
	RootName   string // root directory name (default "records")
	Seed       int64  // random seed for reservoir sampling (0 = deterministic)
}

// DefaultInferConfig returns sensible defaults.
func DefaultInferConfig() InferConfig {
	return InferConfig{
		SampleSize: 1000,
		RootName:   "records",
	}
}

// Inferrer orchestrates FCA-based schema inference.
type Inferrer struct {
	Config InferConfig
}

// InferFromRecords infers a topology from pre-loaded records.
func (inf *Inferrer) InferFromRecords(records []any) (*api.Topology, error) {
	if len(records) == 0 {
		return &api.Topology{Version: "v1"}, nil
	}

	// Sample if needed
	sampled := records
	if len(records) > inf.Config.SampleSize && inf.Config.SampleSize > 0 {
		sampled = reservoirSample(records, inf.Config.SampleSize, inf.Config.Seed)
	}

	// Build formal context
	ctx := BuildContextFromRecords(sampled)

	// Compute concept lattice
	concepts := NextClosure(ctx)

	// Check if this looks like AST data (has "type" attributes)
	isAST := false
	for _, attr := range ctx.Attributes {
		if attr.Name == "type" || (attr.Kind == ScaledValue && attr.Field == "type") {
			isAST = true
			break
		}
	}

	if isAST {
		return ProjectAST(concepts, ctx), nil
	}

	// Project into topology
	config := ProjectConfig{RootName: inf.Config.RootName}
	if config.RootName == "" {
		config.RootName = "records"
	}
	return Project(concepts, ctx, config), nil
}

// InferFromTreeSitter infers a topology from a parsed Tree-sitter AST.
func (inf *Inferrer) InferFromTreeSitter(root *sitter.Node) (*api.Topology, error) {
	records := ingest.FlattenAST(root)
	return inf.InferFromRecords(records)
}

// InferFromSQLite infers a topology by streaming records from a SQLite database.
// Uses reservoir sampling to keep memory bounded.
func (inf *Inferrer) InferFromSQLite(dbPath string) (*api.Topology, error) {
	sampleSize := inf.Config.SampleSize
	if sampleSize <= 0 {
		sampleSize = 1000
	}

	reservoir := make([]any, 0, sampleSize)
	rng := rand.New(rand.NewSource(inf.Config.Seed))
	count := 0

	err := ingest.StreamSQLite(dbPath, func(_ string, record any) error {
		if count < sampleSize {
			reservoir = append(reservoir, record)
		} else {
			j := rng.Intn(count + 1)
			if j < sampleSize {
				reservoir[j] = record
			}
		}
		count++
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("sample sqlite: %w", err)
	}

	if count == 0 {
		return &api.Topology{Version: "v1"}, nil
	}

	return inf.InferFromRecords(reservoir)
}

// InferFromSQLiteJSON is like InferFromSQLite but works with raw JSON strings.
// Used when records need custom parsing.
func (inf *Inferrer) InferFromSQLiteJSON(dbPath string) (*api.Topology, error) {
	sampleSize := inf.Config.SampleSize
	if sampleSize <= 0 {
		sampleSize = 1000
	}

	type rawRecord struct {
		raw string
	}

	rawReservoir := make([]rawRecord, 0, sampleSize)
	rng := rand.New(rand.NewSource(inf.Config.Seed))
	count := 0

	err := ingest.StreamSQLiteRaw(dbPath, func(_, raw string) error {
		if count < sampleSize {
			rawReservoir = append(rawReservoir, rawRecord{raw: raw})
		} else {
			j := rng.Intn(count + 1)
			if j < sampleSize {
				rawReservoir[j] = rawRecord{raw: raw}
			}
		}
		count++
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("sample sqlite raw: %w", err)
	}

	// Parse sampled records
	records := make([]any, len(rawReservoir))
	for i, r := range rawReservoir {
		var parsed any
		if err := json.Unmarshal([]byte(r.raw), &parsed); err != nil {
			return nil, fmt.Errorf("parse sample %d: %w", i, err)
		}
		records[i] = parsed
	}

	return inf.InferFromRecords(records)
}

// reservoirSample performs reservoir sampling on a slice.
func reservoirSample(records []any, k int, seed int64) []any {
	if len(records) <= k {
		return records
	}
	rng := rand.New(rand.NewSource(seed))
	reservoir := make([]any, k)
	copy(reservoir, records[:k])
	for i := k; i < len(records); i++ {
		j := rng.Intn(i + 1)
		if j < k {
			reservoir[j] = records[i]
		}
	}
	return reservoir
}
