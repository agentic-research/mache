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
	SampleSize int               // max records to sample (default 1000)
	RootName   string            // root directory name (default "records")
	Seed       int64             // random seed for reservoir sampling (0 = deterministic)
	Method     string            // "fca" (default) or "greedy"
	MaxDepth   int               // max depth for greedy inference (default 5)
	Hints      map[string]string // user-provided type hints
	Language   string            // language hint for generated nodes (e.g., "go", "terraform")
}

// DefaultInferConfig returns sensible defaults.
func DefaultInferConfig() InferConfig {
	return InferConfig{
		SampleSize: 1000,
		RootName:   "records",
		Method:     "greedy",
		MaxDepth:   5,
		Hints:      make(map[string]string),
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

	if inf.Config.Method == "greedy" {
		config := ProjectConfig{
			RootName: inf.Config.RootName,
			MaxDepth: inf.Config.MaxDepth,
			Hints:    inf.Config.Hints,
		}
		if config.RootName == "" {
			config.RootName = "records"
		}
		if config.MaxDepth <= 0 {
			config.MaxDepth = 5
		}
		return InferGreedy(sampled, config), nil
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

	// Build projection config with language hint
	config := ProjectConfig{
		RootName: inf.Config.RootName,
		MaxDepth: inf.Config.MaxDepth,
		Hints:    inf.Config.Hints,
		Language: inf.Config.Language,
	}
	if config.RootName == "" {
		config.RootName = "records"
	}

	if isAST {
		return ProjectAST(concepts, ctx, config), nil
	}

	return Project(concepts, ctx, config), nil
}

// InferFromTreeSitter infers a topology from a parsed Tree-sitter AST.
// Always uses the FCA path (ProjectAST) because the greedy path generates
// JSONPath selectors which are incompatible with tree-sitter ingestion.
// ProjectAST generates proper S-expression selectors for tree-sitter queries.
func (inf *Inferrer) InferFromTreeSitter(root *sitter.Node) (*api.Topology, error) {
	records := ingest.FlattenAST(root)

	// Force FCA method for AST data — greedy generates JSONPath selectors
	// that fail when the engine tries to use them as tree-sitter queries.
	saved := inf.Config.Method
	inf.Config.Method = "fca"
	defer func() { inf.Config.Method = saved }()

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

// InferMultiLanguage infers a multi-language schema from per-language record sets.
// Creates a namespace node for each language and returns a unified topology.
func (inf *Inferrer) InferMultiLanguage(recordsByLang map[string][]any) (*api.Topology, error) {
	if len(recordsByLang) == 0 {
		return &api.Topology{Version: "v1"}, nil
	}

	// Sort language names for deterministic output
	languages := make([]string, 0, len(recordsByLang))
	for lang := range recordsByLang {
		languages = append(languages, lang)
	}
	// Sort to ensure deterministic schema generation
	for i := 0; i < len(languages); i++ {
		for j := i + 1; j < len(languages); j++ {
			if languages[i] > languages[j] {
				languages[i], languages[j] = languages[j], languages[i]
			}
		}
	}

	// Infer schema for each language
	var rootNodes []api.Node
	for _, langName := range languages {
		records := recordsByLang[langName]
		if len(records) == 0 {
			continue
		}

		// Run FCA inference for this language
		saved := inf.Config.Method
		savedLang := inf.Config.Language
		inf.Config.Method = "fca"
		inf.Config.Language = langName
		subSchema, err := inf.InferFromRecords(records)
		inf.Config.Method = saved
		inf.Config.Language = savedLang

		if err != nil {
			return nil, fmt.Errorf("infer %s: %w", langName, err)
		}

		// Skip languages where FCA produced no useful schema
		if len(subSchema.Nodes) == 0 {
			fmt.Printf("  Warning: %s FCA produced empty schema, files will go to _project_files/\n", langName)
			continue
		}

		// Create language namespace node
		langNode := api.Node{
			Name:     langName,
			Selector: "$", // Passthrough selector
			Language: langName,
			Children: subSchema.Nodes,
		}
		rootNodes = append(rootNodes, langNode)
	}

	return &api.Topology{
		Version: "v1",
		Nodes:   rootNodes,
	}, nil
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
