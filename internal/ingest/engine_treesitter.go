package ingest

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agentic-research/mache/graph"
)

// ingestSourceTree projects a source directory. The AST was pre-parsed by
// ley-line into the engine's `_ast` db; mache runs NO tree-sitter (ADR-0012
// step 4) and never reads a source file's bytes — every construct and every
// file-level extract comes from the ASTWalker querying that db. The walk
// therefore only resolves paths; projection is sequential (processNode + store
// mutations) in a deterministic order.
//
// This used to be a worker pool that read every file's content in parallel —
// a vestige of the in-process tree-sitter era, when the bytes were the parser's
// input. After ADR-0012 nothing consumed them, yet the whole corpus sat in the
// results slice for the length of the projection (mache-95a33d).
func (e *Engine) ingestSourceTree(rootPath string) error {
	var results []parsedSourceFile
	var rawFiles []struct {
		path    string
		modTime time.Time
	}
	walkErr := filepath.WalkDir(rootPath, func(p string, d os.DirEntry, err error) error {
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
		if !ok {
			if !isBinaryFile(p) {
				rawFiles = append(rawFiles, struct {
					path    string
					modTime time.Time
				}{p, info.ModTime()})
			}
			return nil
		}

		// Resolved (symlink-evaluated) path is the key RecordFile stores and
		// the _ast source_id derives from, so resolve it once here.
		absPath, err := filepath.Abs(p)
		if err != nil {
			return err // coverage:ignore
		} // coverage:ignore
		realPath, err := filepath.EvalSymlinks(absPath)
		if err != nil {
			realPath = absPath // coverage:ignore
		} // coverage:ignore

		// Skip unchanged files when an index is available.
		if e.fileIndex != nil {
			if entry, ok := e.fileIndex[realPath]; ok { // coverage:ignore
				if entry.ModTime.Equal(info.ModTime()) && entry.Size == info.Size() { // coverage:ignore
					return nil // unchanged, skip re-projecting // coverage:ignore
				} // coverage:ignore
			}
		}
		results = append(results, parsedSourceFile{
			job: sourceFileJob{
				path:     p,
				langName: langName,
				modTime:  info.ModTime(),
			},
			realPath: realPath,
		})
		return nil
	})
	if walkErr != nil {
		return walkErr // coverage:ignore
	} // coverage:ignore

	// Sort by walk path. Dedup suffixes (e.g., init.from_b_go) depend on the
	// order files are projected, and this lexical order over the full path is
	// the order every existing build has used — it is NOT WalkDir's order
	// (which sorts per directory: "a/x.go" walks before "a-b.go"), so it stays
	// an explicit sort rather than relying on walk order.
	sort.Slice(results, func(i, j int) bool {
		return results[i].job.path < results[j].job.path
	})

	var firstErr error
	for i := range results {
		if (i+1)%1000 == 0 {
			log.Printf("Ingested %d/%d files...", i+1, len(results)) // coverage:ignore
		} // coverage:ignore
		if err := e.processSourceFileResult(&results[i]); err != nil {
			if firstErr == nil { // coverage:ignore
				firstErr = err // coverage:ignore
			} // coverage:ignore
		}
	}

	// Process raw (non-tree-sitter) files sequentially (cheap, no parsing).
	for _, rf := range rawFiles {
		if err := e.ingestRawFileUnder(rf.path, "_project_files", rf.modTime); err != nil {
			if firstErr == nil { // coverage:ignore
				firstErr = err // coverage:ignore
			} // coverage:ignore
		}
	}

	if len(results) > 0 {
		log.Printf("Ingested %d source files total.", len(results))
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
// ingestSourceFile and ingestSourceTree delegate here to avoid divergent
// logic. The caller populates the parsedSourceFile struct (path); construct
// resolution and every file-level extract come from the ASTWalker querying
// the ley-line-parsed `_ast` db — the file's bytes are never read.
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
	// Every walker query below is keyed by this file's sourceID, and nothing
	// reads the engine walker's caches after the file is projected (serve-time
	// callee extraction builds its own walkers). Holding them for the whole
	// build was 650 MB of a 1.7 GB peak on a 929-file repo (mache-95a33d).
	// ReIngestFile invalidates before re-ingesting, so this adds no reload on
	// the daemon path. Deferred so the _project_files early returns evict too.
	defer w.InvalidateSource(sourceID)
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

	result := &parsedSourceFile{
		job: sourceFileJob{
			path:     path,
			langName: langName,
			modTime:  modTime,
		},
		realPath: realPath,
	}

	return e.processSourceFileResult(result)
}
