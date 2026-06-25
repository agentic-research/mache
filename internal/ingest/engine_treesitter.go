package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agentic-research/mache/internal/graph"
	sitter "github.com/smacker/go-tree-sitter"
)

// ingestTreeSitterParallel processes a tree-sitter source directory using
// parallel file parsing. Phase 1 walks the directory and sends file jobs to
// a worker pool that performs the CPU-heavy tree-sitter parsing in parallel.
// Phase 2 applies the parsed results sequentially (processNode + store mutations).
func (e *Engine) ingestTreeSitterParallel(rootPath string) error {
	numWorkers := runtime.NumCPU()
	jobs := make(chan treeSitterJob, numWorkers*4)
	parsed := make(chan parsedTreeSitterFile, numWorkers*4)

	// Phase 1: Workers parse files in parallel (CPU-bound tree-sitter parsing).
	var workerWg sync.WaitGroup
	for range numWorkers {
		workerWg.Go(func() {
			// Pin to one OS thread for the lifetime of the worker.
			// tree-sitter's CGO bridge is sensitive to goroutine
			// migration mid-call: when the Go runtime preempts and
			// resumes a goroutine on a different OS thread while
			// CGO is in flight, we've seen sporadic SIGSEGVs in
			// internal/ingest tests on ubuntu-latest (mache-2y9w).
			// LockOSThread isolates each parser/cursor pair to a
			// stable thread; UnlockOSThread on exit lets the runtime
			// reclaim the thread when the worker goroutine ends.
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			parser := sitter.NewParser()
			for job := range jobs {
				result := parsedTreeSitterFile{job: job}
				absPath, err := filepath.Abs(job.path)
				if err != nil {
					result.readErr = err // coverage:ignore
					parsed <- result     // coverage:ignore
					continue             // coverage:ignore
				}
				result.realPath, err = filepath.EvalSymlinks(absPath)
				if err != nil {
					result.realPath = absPath // coverage:ignore
				} // coverage:ignore

				result.content, err = os.ReadFile(result.realPath)
				if err != nil {
					result.readErr = err // coverage:ignore
					parsed <- result     // coverage:ignore
					continue             // coverage:ignore
				}

				// S4 (mache-33de70): the CGO tree-sitter parse + sitter
				// file-level extracts run ONLY when there's no ASTWalker
				// backend. When a pre-parsed _ast db is mounted, astWalker
				// is set and Phase-2 serves context/imports/file-level-refs
				// from SQL — so we skip the parse entirely and no CGO runs
				// in ingest (this is also where the mache-2y9w SIGSEGV
				// source disappears for the AST path). Parity between the
				// two paths' context/imports/refs is asserted by
				// TestASTQueryParity (projection) + the direct extractor
				// parity tests.
				if e.astWalker == nil {
					parser.SetLanguage(job.lang)
					tree, err := parser.ParseCtx(context.Background(), nil, result.content)
					switch {
					case err != nil: // coverage:ignore
						result.parseErr = err // coverage:ignore
					case tree == nil: // coverage:ignore
						// ParseCtx is documented to return (nil, nil) for
						// non-error empty inputs. Treat as a no-op parse so
						// downstream tree.RootNode() can't nil-deref under
						// the parallel worker (which would surface as a
						// SIGSEGV in CGO via the tree-sitter call stack —
						// the same class of failure as mache-2y9w).
						result.parseErr = fmt.Errorf("tree-sitter returned nil tree") // coverage:ignore
					default:
						result.tree = tree
						// Extract context (imports, globals) — CPU-bound query execution.
						if ctxBytes, err := e.sitterWalker.ExtractContext(
							tree.RootNode(), result.content, job.lang, job.langName,
						); err == nil {
							result.context = ctxBytes
						}
						// Extract structured imports (Go) — avoids regex re-parsing at query time.
						if job.langName == "go" {
							result.imports = e.sitterWalker.ExtractGoImports(tree.RootNode(), result.content, job.lang)
						}
						// Extract file-level refs (identifiers in positions
						// that per-scope ExtractCalls can't see, e.g. Go
						// top-level cobra var declarations — mache-02r9).
						// nil-OK for languages with no registered query.
						if refs, err := e.sitterWalker.ExtractFileLevelRefs(
							tree.RootNode(), result.content, job.lang, job.langName,
						); err == nil {
							result.fileLevelRefs = refs
						}
					}
				}
				parsed <- result
			}
		})
	}

	// Walk directory and send jobs. Non-tree-sitter files (raw files) are
	// processed inline since they're cheap (just file copy, no parsing).
	var walkErr error
	var rawFiles []struct {
		path    string
		modTime time.Time
	}
	var fileCount atomic.Int64
	go func() {
		defer close(jobs)
		walkErr = filepath.WalkDir(rootPath, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err // coverage:ignore
			} // coverage:ignore
			if d.IsDir() {
				if p != rootPath && ShouldSkipDir(d.Name()) {
					return filepath.SkipDir
				}
				if e.gitignore != nil && p != rootPath {
					rel, relErr := filepath.Rel(rootPath, p)
					if relErr == nil {
						rel = filepath.ToSlash(rel)
						if e.gitignore.Match(rel, true) {
							return filepath.SkipDir
						}
					}
				}
				return nil
			}
			if e.gitignore != nil {
				rel, relErr := filepath.Rel(rootPath, p)
				if relErr == nil {
					rel = filepath.ToSlash(rel)
					if e.gitignore.Match(rel, false) {
						return nil
					}
				}
			}
			if d.Type()&os.ModeSymlink != 0 {
				target, err := os.Stat(p)         // coverage:ignore
				if err == nil && target.IsDir() { // coverage:ignore
					return nil // coverage:ignore
				} // coverage:ignore
			}
			info, err := d.Info()
			if err != nil {
				return err // coverage:ignore
			} // coverage:ignore
			if ShouldSkipFile(p, info.Size()) {
				return nil
			}

			ext := filepath.Ext(p)
			lang, langName := langForExt(ext)
			if lang != nil {
				// Skip unchanged files when an index is available.
				// Use resolved (symlink-evaluated) path for consistent cache key,
				// matching RecordFile which stores result.realPath.
				if e.fileIndex != nil {
					lookupPath := p                                            // coverage:ignore
					if resolved, err := filepath.EvalSymlinks(p); err == nil { // coverage:ignore
						lookupPath = resolved // coverage:ignore
					} // coverage:ignore
					if entry, ok := e.fileIndex[lookupPath]; ok { // coverage:ignore
						if entry.ModTime.Equal(info.ModTime()) && entry.Size == info.Size() { // coverage:ignore
							return nil // unchanged, skip re-parsing // coverage:ignore
						} // coverage:ignore
					}
				}
				fileCount.Add(1)
				jobs <- treeSitterJob{
					path:     p,
					lang:     lang,
					langName: langName,
					modTime:  info.ModTime(),
				}
			} else {
				if !isBinaryFile(p) {
					rawFiles = append(rawFiles, struct {
						path    string
						modTime time.Time
					}{p, info.ModTime()})
				}
			}
			return nil
		})
	}()

	// Phase 2: Collect all parsed results, then sort by path for deterministic
	// processing order. Dedup suffixes (e.g., init.from_b_go) depend on the
	// order files are processed — alphabetical matches filepath.WalkDir behavior.
	var firstErr error
	var results []parsedTreeSitterFile
	// Wait for workers to finish in a separate goroutine so we can collect results.
	doneCh := make(chan struct{})
	go func() {
		workerWg.Wait()
		close(parsed)
		close(doneCh)
	}()

	for result := range parsed {
		results = append(results, result)
	}

	// Sort by walk path to match filepath.WalkDir's lexical order.
	sort.Slice(results, func(i, j int) bool {
		return results[i].job.path < results[j].job.path
	})

	processed := 0
	for i := range results {
		processed++
		if processed%1000 == 0 {
			log.Printf("Ingested %d/%d files...", processed, fileCount.Load()) // coverage:ignore
		} // coverage:ignore

		if results[i].readErr != nil {
			if firstErr == nil { // coverage:ignore
				firstErr = results[i].readErr // coverage:ignore
			} // coverage:ignore
			continue // coverage:ignore
		}

		if err := e.processTreeSitterResult(&results[i]); err != nil {
			if firstErr == nil { // coverage:ignore
				firstErr = err // coverage:ignore
			} // coverage:ignore
		}
	}

	// Wait for walk to complete.
	<-doneCh
	if walkErr != nil {
		return walkErr // coverage:ignore
	} // coverage:ignore

	// Process raw (non-tree-sitter) files sequentially (cheap, no parsing).
	for _, rf := range rawFiles {
		if err := e.ingestRawFileUnder(rf.path, "_project_files", rf.modTime); err != nil {
			if firstErr == nil { // coverage:ignore
				firstErr = err // coverage:ignore
			} // coverage:ignore
		}
	}

	if fileCount.Load() > 0 {
		log.Printf("Ingested %d source files total (%d workers).", processed, numWorkers)
	}

	return firstErr
}

// processTreeSitterResult handles everything AFTER parsing for a single tree-sitter file.
// Both ingestTreeSitter (sequential) and ingestTreeSitterParallel (phase 2) delegate here
// to avoid divergent logic. The caller is responsible for populating the parsedTreeSitterFile
// struct — workers do it in parallel, the sequential path does it inline.
//
// Steps:
//  1. Parse error → BROKEN_ node with SHA256(path) ID (no collision)
//  2. Filter schema nodes by language
//  3. No applicable nodes → route to _project_files
//  4. Extract address refs
//  5. processNode for each applicable schema node
//  6. Invalid query error → route to _project_files
//  7. No buffered nodes → route to _project_files
//  8. Atomic swap via ReplaceFileNodes
//  9. RecordFile for incremental re-ingestion
func (e *Engine) processTreeSitterResult(result *parsedTreeSitterFile) error {
	// 1. Handle parse errors — use SHA256(path) for unique BROKEN_ IDs.
	if result.parseErr != nil {
		log.Printf("ingest: parse failed for %s (using raw fallback): %v", result.job.path, result.parseErr) // coverage:ignore
		pathForID := result.realPath                                                                         // coverage:ignore
		if pathForID == "" {                                                                                 // coverage:ignore
			pathForID = result.job.path // coverage:ignore
		} // coverage:ignore
		sum := sha256.Sum256([]byte(pathForID))               // coverage:ignore
		fallbackID := "BROKEN_" + hex.EncodeToString(sum[:8]) // coverage:ignore
		fileNode := &graph.Node{                              // coverage:ignore
			ID:      fallbackID,         // coverage:ignore
			Mode:    0o444,              // coverage:ignore
			ModTime: result.job.modTime, // coverage:ignore
			Data:    result.content,     // coverage:ignore
			Origin: &graph.SourceOrigin{ // coverage:ignore
				FilePath:  result.realPath,             // coverage:ignore
				StartByte: 0,                           // coverage:ignore
				EndByte:   uint32(len(result.content)), // coverage:ignore
			}, // coverage:ignore
		} // coverage:ignore
		e.Store.AddNode(fileNode) // coverage:ignore
		e.Store.AddRoot(fileNode) // coverage:ignore
		return nil                // coverage:ignore
	}

	// Select walker: ASTWalker (pure Go, SQL) when available, else SitterWalker (CGO).
	var w Walker
	var root any
	if e.astWalker != nil {
		w = e.astWalker
		sourceID := filepath.Base(result.job.path)
		root = ASTRoot{
			DB:           e.astWalker.db,
			SourceID:     sourceID,
			ParentPrefix: "",
		}
		// S4: Phase-1 ran no CGO parse on this path, so the file-level
		// extracts the sitter worker would have done are served here from
		// SQL instead. Each mirrors the sitter extract it replaces; the
		// projection parity gate asserts the results match byte-for-byte.
		if ctxBytes, err := e.astWalker.ExtractContext(result.job.path, result.job.langName); err == nil {
			result.context = ctxBytes
		}
		if result.job.langName == "go" {
			if imp, err := e.astWalker.ExtractGoImports(sourceID); err == nil {
				result.imports = imp
			}
		}
		if refs, err := e.astWalker.ExtractFileLevelRefs(sourceID, result.job.langName); err == nil {
			result.fileLevelRefs = refs
		}
	} else {
		sw := e.sitterWalker
		if sw == nil {
			sw = NewSitterWalker() // coverage:ignore
			defer sw.Close()       // coverage:ignore
		} // coverage:ignore
		w = sw
		root = SitterRoot{
			Node:     result.tree.RootNode(),
			FileRoot: result.tree.RootNode(),
			Source:   result.content,
			Lang:     result.job.lang,
			LangName: result.job.langName,
		}
	}

	bt := &bufferingTarget{IngestionTarget: e.Store}
	sourceFile := filepath.Base(result.job.path)

	// 2. Filter schema nodes by language.
	applicableNodes := filterNodesByLanguage(e.Schema.Nodes, result.job.langName)

	// 3. No applicable schema nodes → route to _project_files/.
	if len(applicableNodes) == 0 {
		return e.ingestRawFileUnder(result.job.path, "_project_files", result.job.modTime) // coverage:ignore
	} // coverage:ignore

	// 4. Extract file-level address refs (e.g., HCL variable declarations).
	// ASTWalker path queries _ast table for the same patterns.
	var fileAddrRefs []string
	switch wt := w.(type) {
	case *SitterWalker:
		if result.tree != nil {
			if addrRefs, err := wt.ExtractAddressRefs(result.tree.RootNode(), result.content, result.job.lang, result.job.langName); err == nil {
				fileAddrRefs = addrRefs
			}
		}
	case *ASTWalker: // coverage:ignore
		if addrRefs, err := wt.ExtractAddressRefs(result.job.path, result.job.langName); err == nil { // coverage:ignore
			fileAddrRefs = addrRefs // coverage:ignore
		} // coverage:ignore
	}
	// File-level refs (mache-02r9: top-level cobra RunE etc.) are
	// emitted with a SENTINEL caller_id rather than merged into
	// every construct's calls. Earlier iterations folded them into
	// fileAddrRefs (per-construct merge), which inflated fan_out_skew
	// — every function in a cobra-using file picked up the cobra
	// callback as a 'callee' even though it doesn't actually call it.
	//
	// The sentinel form keeps the alive set correct for dead_code
	// (token-only check) without polluting any rule that aggregates
	// by caller. fan_out_skew explicitly skips sentinel rows.
	const fileLevelSentinelPrefix = "_file_level:"
	if len(result.fileLevelRefs) > 0 {
		sentinel := fileLevelSentinelPrefix + result.realPath
		for _, token := range result.fileLevelRefs {
			if err := bt.AddRef(token, sentinel); err != nil {
				log.Printf("file-level ref %q: %v", token, err) // coverage:ignore
			} // coverage:ignore
		}
	}

	// 5. processNode for each applicable schema node.
	for _, nodeSchema := range applicableNodes {
		if err := e.processNode(nodeSchema, w, root, "", sourceFile, result.realPath, result.job.modTime, bt, result.context, fileAddrRefs, nil, result.imports); err != nil {
			// 6. Invalid query → route to _project_files/.
			if strings.Contains(err.Error(), "invalid query") {
				e.mu.Lock()
				e.routedFiles[result.job.langName]++
				e.mu.Unlock()
				return e.ingestRawFileUnder(result.job.path, "_project_files", result.job.modTime)
			}
			return fmt.Errorf("failed to process schema node %s: %w", nodeSchema.Name, err) // coverage:ignore
		}
	}

	// 7. No nodes produced → route to _project_files/.
	if len(bt.bufferedNodes) == 0 {
		return e.ingestRawFileUnder(result.job.path, "_project_files", result.job.modTime) // coverage:ignore
	} // coverage:ignore

	// 8. Atomic swap of file nodes.
	if ms, ok := e.Store.(*graph.MemoryStore); ok {
		ms.ReplaceFileNodes(result.realPath, bt.bufferedNodes)
	} else {
		e.Store.DeleteFileNodes(result.realPath) // coverage:ignore
		for _, n := range bt.bufferedNodes {     // coverage:ignore
			e.Store.AddNode(n) // coverage:ignore
		} // coverage:ignore
	}

	// 9. Record file metadata for incremental re-ingestion + coverage row
	//    for ADR-0013's _index_coverage table (mention-fidelity, since
	//    tree-sitter is the L_0 producer in the fidelity poset). LSP and
	//    SSA producers will write their own (binding/reachability) rows
	//    for the same source_id.
	if sw, ok := e.Store.(*SQLiteWriter); ok {
		info, err := os.Stat(result.realPath) // coverage:ignore
		if err == nil {                       // coverage:ignore
			sw.RecordFile(result.realPath, info.ModTime(), info.Size())                         // coverage:ignore
			sw.RecordIndexCoverage(result.realPath, "tree-sitter", "mention", time.Now(), true) // coverage:ignore
		} // coverage:ignore
	}

	return nil
}

// ingestTreeSitter parses and processes a single source file. Used by
// ReIngestFile and the synchronous dispatch in ingestFile.
func (e *Engine) ingestTreeSitter(path string, grammar *sitter.Language, langName string, modTime time.Time) error {
	// Pin to one OS thread for the lifetime of this call, mirroring
	// the parallel worker fix in PR #257. tree-sitter's CGO bridge
	// is sensitive to goroutine migration mid-call: when the Go
	// runtime preempts and resumes a goroutine on a different OS
	// thread while CGO is in flight, sporadic SIGSEGVs appear in
	// internal/ingest tests (mache-2y9w). The parallel path got
	// LockOSThread; this synchronous single-file path — used by
	// ReIngestFile and the dispatch loop's default branch — was
	// missed and kept producing CGO SIGSEGV reruns on PRs that
	// touched code unrelated to ingestion (#284, #292, #294, #297).
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	absPath, err := filepath.Abs(path)
	if err != nil {
		return err // coverage:ignore
	} // coverage:ignore
	realPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		realPath = absPath // coverage:ignore
	} // coverage:ignore

	if _, err := ensureFile(realPath, "a source file"); err != nil {
		return err // coverage:ignore
	} // coverage:ignore

	content, err := os.ReadFile(realPath)
	if err != nil {
		return err // coverage:ignore
	} // coverage:ignore

	result := &parsedTreeSitterFile{
		job: treeSitterJob{
			path:     path,
			lang:     grammar,
			langName: langName,
			modTime:  modTime,
		},
		realPath: realPath,
		content:  content,
	}

	// S4 (mache-33de70): parse + sitter context/imports extraction only
	// when there's no ASTWalker backend. With astWalker set, no CGO runs
	// and processTreeSitterResult serves context/imports/file-level-refs
	// from SQL. Mirrors the parallel-worker gate above.
	if e.astWalker == nil {
		parser := sitter.NewParser()
		parser.SetLanguage(grammar)
		tree, parseErr := parser.ParseCtx(context.Background(), nil, content)
		result.tree = tree
		result.parseErr = parseErr

		// Extract context (imports, globals) when parse succeeded.
		if parseErr == nil {
			walker := e.sitterWalker
			if walker == nil {
				walker = NewSitterWalker() // coverage:ignore
				defer walker.Close()       // coverage:ignore
			} // coverage:ignore
			if ctxBytes, err := walker.ExtractContext(tree.RootNode(), content, grammar, langName); err == nil {
				result.context = ctxBytes
			}
			if langName == "go" {
				result.imports = walker.ExtractGoImports(tree.RootNode(), content, grammar)
			}
		}
	}

	return e.processTreeSitterResult(result)
}
