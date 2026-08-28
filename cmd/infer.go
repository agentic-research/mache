package cmd

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/lang"
	"github.com/agentic-research/mache/internal/lattice"
	"github.com/agentic-research/mache/internal/leylinegraph"
)

// inferSampleRecords bounds how many _ast records feed FCA inference per
// language. Replaces the old 200-file sampling cap from the in-process
// tree-sitter path: records (AST nodes) are the unit leyline-backed
// inference streams, so the bound is expressed in records.
const inferSampleRecords = 50000

// sourceCodePresets maps language names to their preset schema keys.
// Derived from the lang registry at init time — adding a language
// to internal/lang automatically adds its preset here.
var sourceCodePresets map[string]string

func init() {
	sourceCodePresets = make(map[string]string)
	for i := range lang.Registry {
		l := &lang.Registry[i]
		if l.PresetSchema != "" {
			sourceCodePresets[l.Name] = l.PresetSchema
		}
	}
}

// detectProjectLanguages walks a directory tree and returns a map of
// language name → file count for all source files found. Skips hidden
// directories, node_modules, target, dist, and build.
func detectProjectLanguages(dir string) (map[string]int, error) {
	counts := make(map[string]int)
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if path != dir && ingest.ShouldSkipDir(filepath.Base(path)) {
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(path)
		if l := lang.ForExt(ext); l != nil {
			counts[l.Name]++
		}
		return nil
	})
	return counts, err
}

// inferDirSchema detects languages in a directory and produces a unified
// Topology using preset schemas where available and FCA inference for the rest.
//
// Hybrid strategy:
//  1. Detect all source languages
//  2. Languages with presets (go, python, sql) → load embedded preset schema
//  3. Remaining languages → sample files + FCA inference
//  4. Merge into one multi-language topology (with namespace nodes if >1 language)
func inferDirSchema(dataPath string) (*api.Topology, error) {
	languageCounts, err := detectProjectLanguages(dataPath)
	if err != nil {
		return nil, fmt.Errorf("language scan: %w", err)
	}
	if len(languageCounts) == 0 {
		log.Printf("No source files found, using passthrough schema")
		return &api.Topology{Version: api.SchemaVersion}, nil
	}

	// Log detected languages
	langs := make([]string, 0, len(languageCounts))
	for l := range languageCounts {
		langs = append(langs, l)
	}
	sort.Strings(langs)
	for _, l := range langs {
		log.Printf("  detected: %s (%d files)", l, languageCounts[l])
	}

	// Split into preset vs inference buckets
	var presetLangs, inferLangs []string
	for _, l := range langs {
		if _, ok := sourceCodePresets[l]; ok {
			presetLangs = append(presetLangs, l)
		} else {
			inferLangs = append(inferLangs, l)
		}
	}

	// Collect nodes from both paths
	var allNodes []api.Node

	// 1. Load preset schemas
	for _, l := range presetLangs {
		presetKey := sourceCodePresets[l]
		topo, err := loadPresetSchema(presetKey)
		if err != nil {
			return nil, fmt.Errorf("load preset %q: %w", presetKey, err)
		}
		if len(langs) == 1 {
			// Single language: return the preset directly (no namespace wrapper)
			return topo, nil
		}
		// Multi-language: wrap in namespace node
		allNodes = append(allNodes, api.Node{
			Name:     l,
			Selector: "$",
			Language: l,
			Children: topo.Nodes,
		})
		log.Printf("  %s: using preset schema", l)
	}

	// 2. FCA inference for remaining languages — leyline-parse the tree ONCE
	// into an _ast database, then run pure-Go inference per language against it.
	if len(inferLangs) > 0 {
		astDB, cleanup, err := leylinegraph.AutoInvokeLeylineParse(dataPath)
		if err != nil {
			return nil, fmt.Errorf("leyline parse for inference: %w", err)
		}
		defer cleanup()

		inferredNodes, err := inferLanguages(astDB, inferLangs, languageCounts)
		if err != nil {
			return nil, fmt.Errorf("inference: %w", err)
		}
		if len(langs) == 1 && len(inferredNodes) > 0 {
			// Single language inferred: return directly (no namespace wrapper)
			return &api.Topology{Version: api.SchemaVersion, Nodes: inferredNodes[0].Children}, nil
		}
		allNodes = append(allNodes, inferredNodes...)
	}

	return &api.Topology{Version: api.SchemaVersion, Nodes: allNodes}, nil
}

// inferLanguages runs pure-Go FCA inference for the given languages against
// a leyline-parsed _ast database (no in-process tree-sitter). Returns
// namespace-wrapped nodes for each language, mirroring the shape
// lattice.InferMultiLanguage produces.
func inferLanguages(astDBPath string, langs []string, languageCounts map[string]int) ([]api.Node, error) {
	var nodes []api.Node
	for _, targetLang := range langs {
		inf := &lattice.Inferrer{
			Config: lattice.InferConfig{
				Method:     "fca",
				SampleSize: inferSampleRecords,
				Language:   targetLang,
			},
		}
		topo, err := inf.InferFromASTDB(astDBPath)
		if err != nil {
			return nil, fmt.Errorf("infer %s: %w", targetLang, err)
		}
		if len(topo.Nodes) == 0 {
			log.Printf("infer: %s FCA produced empty schema, files will go to _project_files/", targetLang)
			continue
		}
		log.Printf("  %s: inferred schema from leyline _ast (%d files)", targetLang, languageCounts[targetLang])
		nodes = append(nodes, api.Node{
			Name:     targetLang,
			Selector: "$", // Passthrough selector
			Language: targetLang,
			Children: topo.Nodes,
		})
	}
	return nodes, nil
}
