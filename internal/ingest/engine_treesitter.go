package ingest

import (
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

	"github.com/agentic-research/mache/graph"
)

// ingestSourceParallel projects a source directory using parallel workers.
// The AST was pre-parsed by ley-line into the engine's `_ast` db; mache runs
// NO tree-sitter (ADR-0012 step 4). Phase 1 walks the directory and reads file
// content in parallel; Phase 2 applies results sequentially (processNode + store
// mutations) with the ASTWalker resolving every construct from SQL.
func (e *Engine) ingestSourceParallel(rootPath string) error {
	numWorkers := runtime.NumCPU()
	jobs := make(chan sourceFileJob, numWorkers*4)
	parsed := make(chan parsedSourceFile, numWorkers*4)

	// Phase 1: Workers read file content in parallel. No CGO, no tree-sitter
	// parser allocation, and no LockOSThread pin — the AST already lives in
	// the `_ast` db, so there is no CGO bridge to keep on a stable OS thread
	// (this is where the historical mache-2y9w SIGSEGV source disappears).
	var workerWg sync.WaitGroup
	for range numWorkers {
		workerWg.Go(func() {
			for job := range jobs {
				result := parsedSourceFile{job: job}
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
				// context/imports/file-level-refs are resolved from SQL in
				// Phase 2 (processSourceFileResult) via the ASTWalker.
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
			langName, ok := langForExt(ext)
			if ok {
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
				jobs <- sourceFileJob{
					path:     p,
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
	var results []parsedSourceFile
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

		if err := e.processSourceFileResult(&results[i]); err != nil {
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

// sourceIDFor returns the _ast/_source key for a file: the path RELATIVE to
// the ingestion root, forward-slashed — exactly how ley-line's `leyline parse`
// keys source_id (e.g. "pkg/a.go", not "a.go"). The ASTWalker query MUST use
// this same key or it finds nothing for any file below the root (mache-30edfa).
//
// Falls back to the base name when RootPath is unset, when RootPath IS the file
// (single-file ingestion, where Rel yields "."), or when the path escapes the
// root — all cases where leyline would also key by the bare name.
func (e *Engine) sourceIDFor(realPath string) string {
	if e.RootPath == "" {
		return filepath.Base(realPath)
	}
	rel, err := filepath.Rel(e.RootPath, realPath)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return filepath.Base(realPath)
	}
	return filepath.ToSlash(rel)
}

// processSourceFileResult handles projection for a single source file. Both
// ingestSourceFile (sequential) and ingestSourceParallel (phase 2) delegate here
// to avoid divergent logic. The caller populates the parsedSourceFile struct
// (path/content); construct resolution and every file-level extract come from
// the ASTWalker querying the ley-line-parsed `_ast` db.
//
// Steps:
//  1. Filter schema nodes by language
//  2. No applicable nodes → route to _project_files
//  3. Extract address refs
//  4. processNode for each applicable schema node
//  5. Invalid query error → route to _project_files
//  6. No buffered nodes → route to _project_files
//  7. Atomic swap via ReplaceFileNodes
//  8. RecordFile for incremental re-ingestion
func (e *Engine) processSourceFileResult(result *parsedSourceFile) error {
	// The ASTWalker is the sole walker (ADR-0012 step 4). Engine.Ingest
	// guarantees it is set before dispatching source files here.
	w := e.astWalker
	// source_id is the path RELATIVE to the ingest root, matching how
	// ley-line keys _ast/_source (mache-30edfa) — NOT filepath.Base,
	// which would miss every file below the root.
	sourceID := e.sourceIDFor(result.realPath)
	root := ASTRoot{
		DB:           w.db,
		SourceID:     sourceID,
		ParentPrefix: "",
	}
	// File-level extracts (context/imports/file-level-refs) are served from
	// SQL — no CGO parse runs. Each mirrors the sitter extract it replaced.
	if ctxBytes, err := w.ExtractContext(sourceID, result.job.langName); err == nil {
		result.context = ctxBytes
	}
	if result.job.langName == "go" {
		if imp, err := w.ExtractGoImports(sourceID); err == nil {
			result.imports = imp
		}
	}
	if refs, err := w.ExtractFileLevelRefs(sourceID, result.job.langName); err == nil {
		result.fileLevelRefs = refs
	}

	bt := &bufferingTarget{IngestionTarget: e.Store}
	sourceFile := filepath.Base(result.job.path)

	// 1. Filter schema nodes by language.
	applicableNodes := filterNodesByLanguage(e.Schema.Nodes, result.job.langName)

	// 2. No applicable schema nodes → route to _project_files/.
	if len(applicableNodes) == 0 {
		return e.ingestRawFileUnder(result.job.path, "_project_files", result.job.modTime) // coverage:ignore
	} // coverage:ignore

	// 3. Extract file-level address refs (e.g., HCL variable declarations)
	// by querying the _ast table for the same patterns.
	var fileAddrRefs []string
	if addrRefs, err := w.ExtractAddressRefs(sourceID, result.job.langName); err == nil {
		fileAddrRefs = addrRefs
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

// ingestSourceFile projects a single source file via the ASTWalker. Used by
// ReIngestFile and the synchronous dispatch in ingestFile. No CGO runs — the
// AST is read from the ley-line-parsed `_ast` db, so there is no tree-sitter
// bridge and no need for the historical LockOSThread pin (mache-2y9w).
func (e *Engine) ingestSourceFile(path, langName string, modTime time.Time) error {
	if e.astWalker == nil {
		return fmt.Errorf("engine: source file %s requires an ASTWalker "+
			"(call SetASTWalker before ingesting source); in-process tree-sitter "+
			"was removed in ADR-0012 step 4", path)
	}

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

	result := &parsedSourceFile{
		job: sourceFileJob{
			path:     path,
			langName: langName,
			modTime:  modTime,
		},
		realPath: realPath,
		content:  content,
	}

	return e.processSourceFileResult(result)
}
