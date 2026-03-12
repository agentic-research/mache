package cmd

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/lattice"
	sitter "github.com/smacker/go-tree-sitter"
)

// sourceCodePresets maps language names (as returned by DetectLanguageFromExt)
// to their preset schema keys. Only source-code presets are included here —
// data-format presets (cli, mcp, mcp-registry) are excluded because they
// don't correspond to detected file extensions.
var sourceCodePresets = map[string]string{
	"go":        "go",
	"python":    "python",
	"rust":      "rust",
	"terraform": "terraform",
	"sql":       "sql",
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
			base := filepath.Base(path)
			if (strings.HasPrefix(base, ".") && path != dir) ||
				base == "target" || base == "node_modules" ||
				base == "dist" || base == "build" {
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(path)
		if langName, _, ok := ingest.DetectLanguageFromExt(ext); ok {
			counts[langName]++
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
	for lang := range languageCounts {
		langs = append(langs, lang)
	}
	sort.Strings(langs)
	for _, lang := range langs {
		log.Printf("  detected: %s (%d files)", lang, languageCounts[lang])
	}

	// Split into preset vs inference buckets
	var presetLangs, inferLangs []string
	for _, lang := range langs {
		if _, ok := sourceCodePresets[lang]; ok {
			presetLangs = append(presetLangs, lang)
		} else {
			inferLangs = append(inferLangs, lang)
		}
	}

	// Collect nodes from both paths
	var allNodes []api.Node

	// 1. Load preset schemas
	for _, lang := range presetLangs {
		presetKey := sourceCodePresets[lang]
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
			Name:     lang,
			Selector: "$",
			Language: lang,
			Children: topo.Nodes,
		})
		log.Printf("  %s: using preset schema", lang)
	}

	// 2. FCA inference for remaining languages
	if len(inferLangs) > 0 {
		inferredNodes, err := inferLanguages(dataPath, inferLangs, languageCounts)
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

// inferLanguages runs parallel tree-sitter sampling + FCA inference for the
// given languages. Returns namespace-wrapped nodes for each language.
func inferLanguages(dataPath string, langs []string, languageCounts map[string]int) ([]api.Node, error) {
	type langResult struct {
		lang    string
		records []any
		err     error
	}

	resultsChan := make(chan langResult, len(langs))
	var wg sync.WaitGroup

	for _, targetLang := range langs {
		wg.Add(1)
		go func(lang string) {
			defer wg.Done()

			var records []any
			fileCount := 0
			sampleLimit := 200

			walkErr := filepath.Walk(dataPath, func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				if info.IsDir() {
					base := filepath.Base(path)
					if (strings.HasPrefix(base, ".") && path != dataPath) ||
						base == "target" || base == "node_modules" ||
						base == "dist" || base == "build" {
						return filepath.SkipDir
					}
					return nil
				}
				if fileCount >= sampleLimit {
					return nil
				}
				if ingest.ShouldSkipFile(path, info.Size()) {
					return nil
				}
				langName, treeLang, ok := ingest.DetectLanguageFromExt(filepath.Ext(path))
				if !ok || langName != lang {
					return nil
				}
				content, readErr := os.ReadFile(path)
				if readErr != nil {
					return nil
				}
				parser := sitter.NewParser()
				parser.SetLanguage(treeLang)
				tree, _ := parser.ParseCtx(context.Background(), nil, content)
				if tree != nil {
					records = append(records, ingest.FlattenASTWithLanguage(tree.RootNode(), langName)...)
				}
				fileCount++
				return nil
			})

			log.Printf("  %s: sampled %d/%d files for inference", lang, fileCount, languageCounts[lang])

			if walkErr != nil {
				resultsChan <- langResult{lang: lang, err: walkErr}
				return
			}
			resultsChan <- langResult{lang: lang, records: records}
		}(targetLang)
	}

	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	// Collect results
	recordsByLang := make(map[string][]any)
	for result := range resultsChan {
		if result.err != nil {
			return nil, fmt.Errorf("scan %s: %w", result.lang, result.err)
		}
		if len(result.records) > 0 {
			recordsByLang[result.lang] = result.records
		}
	}

	if len(recordsByLang) == 0 {
		return nil, nil
	}

	// Run FCA inference
	inf := &lattice.Inferrer{
		Config: lattice.InferConfig{Method: "fca"},
	}

	topo, err := inf.InferMultiLanguage(recordsByLang)
	if err != nil {
		return nil, err
	}

	totalRecords := 0
	for _, recs := range recordsByLang {
		totalRecords += len(recs)
	}
	log.Printf("  inferred schema from %d records (%d languages)", totalRecords, len(recordsByLang))

	return topo.Nodes, nil
}
