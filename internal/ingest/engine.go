package ingest

import (
	"log"
	"os"
	"path/filepath"
	"sync"
	"text/template"
	"time"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
	machetmpl "github.com/agentic-research/mache/internal/template"
	sitter "github.com/smacker/go-tree-sitter"
)

const inlineThreshold = 4096

// IngestionTarget combines Graph reading with writing capabilities.
type IngestionTarget interface {
	graph.Graph
	AddNode(n *graph.Node)
	AddRoot(n *graph.Node)
	AddRef(token, nodeID string) error
	AddDef(token, dirID string) error
	DeleteFileNodes(filePath string)
	AddFileChildren(parent *graph.Node, files []*graph.Node)
}

// Engine drives the ingestion process.
type Engine struct {
	Schema           *api.Topology
	Store            IngestionTarget
	RootPath         string // absolute path to the root of the ingestion
	RespectGitignore bool   // when true, skip files matching .gitignore patterns (default: true)
	routedFiles      map[string]int
	childSeen        map[string]map[string]bool // parentID → set of child IDs (O(1) dedup)
	gitignore        *gitignoreMatcher          // loaded from .gitignore when RespectGitignore is true
	sitterWalker     *SitterWalker              // shared across files for query cache reuse
	astWalker        *ASTWalker                 // SQL-backed walker for ley-line pre-parsed .db files
	fileIndex        map[string]FileIndexEntry  // cached file metadata for incremental re-ingestion
	mu               sync.Mutex

	// diagramOnce guards lazy computation of cachedCommunities + cachedRefs.
	diagramOnce       sync.Once
	cachedCommunities *graph.CommunityResult
	cachedRefs        map[string][]string
	// diagramFuncMapOnce guards building of diagramFuncMap (safe for concurrent use).
	diagramFuncMapOnce sync.Once
	diagramFuncMap     template.FuncMap
	diagramTmplCache   sync.Map // template string -> *template.Template
}

// --- Parallel tree-sitter ingestion types ---

// treeSitterJob represents a source file to parse with tree-sitter.
type treeSitterJob struct {
	path     string
	lang     *sitter.Language
	langName string
	modTime  time.Time
}

// parsedTreeSitterFile is the result of tree-sitter parsing (parallel or sequential).
// Contains the pre-parsed AST and file content, ready for processTreeSitterResult.
type parsedTreeSitterFile struct {
	job           treeSitterJob
	realPath      string
	content       []byte
	tree          *sitter.Tree
	context       []byte            // extracted imports/globals context
	imports       map[string]string // structured imports: alias → path (Go only, nil for others)
	fileLevelRefs []string          // identifiers captured at the file root (Go: top-level cobra refs etc., mache-02r9)
	parseErr      error             // non-nil if tree-sitter parsing failed
	readErr       error             // non-nil if file read failed
}

func NewEngine(schema *api.Topology, store IngestionTarget) *Engine {
	return &Engine{
		Schema:           schema,
		Store:            store,
		RespectGitignore: true,
		routedFiles:      make(map[string]int),
		childSeen:        make(map[string]map[string]bool),
	}
}

// SetASTWalker configures the engine to use a SQL-backed ASTWalker for
// tree-sitter schema selectors instead of CGO SitterWalker. When set,
// source file parsing is skipped — the ASTWalker queries pre-parsed
// _ast/_source tables from a ley-line .db.
func (e *Engine) SetASTWalker(w *ASTWalker) { // coverage:ignore
	if err := w.EnsureIndexes(); err != nil { // coverage:ignore
		log.Printf("ASTWalker: index creation failed (queries will use full scan): %v", err) // coverage:ignore
	} // coverage:ignore
	e.astWalker = w // coverage:ignore
} // coverage:ignore

// GitignoreMatcher matches paths against .gitignore-style rules.
type GitignoreMatcher interface {
	Match(rel string, isDir bool) bool
}

// Gitignore returns the gitignore matcher loaded during Ingest, or nil if none
// was loaded. Pass this to WithGitignore when creating a Watcher so the watcher
// skips the same directories the engine does.
func (e *Engine) Gitignore() GitignoreMatcher { // coverage:ignore
	if e.gitignore == nil { // coverage:ignore
		return nil // coverage:ignore
	} // coverage:ignore
	return e.gitignore // coverage:ignore
} // coverage:ignore

// SetFileIndex sets a cached file index for incremental re-ingestion.
// Files matching (path, mtime, size) will be skipped during ingestion.
func (e *Engine) SetFileIndex(index map[string]FileIndexEntry) { // coverage:ignore
	e.fileIndex = index // coverage:ignore
} // coverage:ignore

// Ingest processes a file or directory.
// Safe to call multiple times — internal dedup state is reset on each call.
func (e *Engine) Ingest(path string) error {
	// Reset dedup state so stale entries from a prior Ingest don't persist.
	e.childSeen = make(map[string]map[string]bool)

	// Create a shared SitterWalker for query cache reuse across files.
	// Compiled tree-sitter queries are identical for all files of the same
	// language, so sharing avoids recompilation (e.g., 50K×20 → ~20).
	e.sitterWalker = NewSitterWalker()
	defer e.sitterWalker.Close()

	absPath, err := filepath.Abs(path)
	if err != nil { // coverage:ignore
		return err // coverage:ignore
	} // coverage:ignore
	realPath, err := filepath.EvalSymlinks(absPath)
	if err != nil { // coverage:ignore
		realPath = absPath // coverage:ignore
	} // coverage:ignore
	e.RootPath = realPath

	info, err := os.Stat(realPath)
	if err != nil { // coverage:ignore
		return err // coverage:ignore
	} // coverage:ignore

	if info.IsDir() {
		// Load .gitignore patterns when enabled (default: true).
		if e.RespectGitignore {
			e.gitignore = LoadGitignore(realPath)
		}

		// Determine which file types this schema can process.
		// Tree-sitter schemas operate on source code (.go, .py);
		// JSONPath schemas operate on data files (.json, .db).
		// Ingesting the wrong type is harmless but wastes time and
		// can produce confusing errors (e.g. S-expression as JSONPath).
		treeSitter := SchemaUsesTreeSitter(e.Schema)

		if treeSitter {
			return e.ingestTreeSitterParallel(realPath)
		}

		return filepath.WalkDir(realPath, func(p string, d os.DirEntry, err error) error { // coverage:ignore
			if err != nil { // coverage:ignore
				return err // coverage:ignore
			} // coverage:ignore
			if d.IsDir() { // coverage:ignore
				if p != realPath && ShouldSkipDir(d.Name()) { // coverage:ignore
					return filepath.SkipDir // coverage:ignore
				} // coverage:ignore
				// Check gitignore for directories
				if e.gitignore != nil && p != realPath { // coverage:ignore
					rel, relErr := filepath.Rel(realPath, p) // coverage:ignore
					if relErr == nil {                       // coverage:ignore
						rel = filepath.ToSlash(rel)       // coverage:ignore
						if e.gitignore.Match(rel, true) { // coverage:ignore
							return filepath.SkipDir // coverage:ignore
						} // coverage:ignore
					} // coverage:ignore
				} // coverage:ignore
				return nil // coverage:ignore
			} // coverage:ignore
			// Check gitignore for files
			if e.gitignore != nil { // coverage:ignore
				rel, relErr := filepath.Rel(realPath, p) // coverage:ignore
				if relErr == nil {                       // coverage:ignore
					rel = filepath.ToSlash(rel)        // coverage:ignore
					if e.gitignore.Match(rel, false) { // coverage:ignore
						return nil // coverage:ignore
					} // coverage:ignore
				} // coverage:ignore
			} // coverage:ignore
			// Skip symlinks to directories (e.g., kodata/templates -> ../templates)
			// WalkDir doesn't follow symlinks, so d.IsDir() is false for them,
			// but os.ReadFile will follow and fail with "is a directory".
			if d.Type()&os.ModeSymlink != 0 { // coverage:ignore
				target, err := os.Stat(p)         // coverage:ignore
				if err == nil && target.IsDir() { // coverage:ignore
					return nil // coverage:ignore
				} // coverage:ignore
			} // coverage:ignore
			// Determine if we should parse or treat as raw based on schema type
			ext := filepath.Ext(p) // coverage:ignore
			info, err := d.Info()  // coverage:ignore
			if err != nil {        // coverage:ignore
				return err // coverage:ignore
			} // coverage:ignore
			if ShouldSkipFile(p, info.Size()) { // coverage:ignore
				return nil // coverage:ignore
			} // coverage:ignore
			shouldParse := false // coverage:ignore
			switch ext {         // coverage:ignore
			case ".json", ".db": // coverage:ignore
				shouldParse = true // coverage:ignore
			} // coverage:ignore

			if shouldParse { // coverage:ignore
				return e.ingestFile(p, info.ModTime()) // coverage:ignore
			} // coverage:ignore
			// Skip binary files (executables, object files, images, etc.)
			if isBinaryFile(p) { // coverage:ignore
				return nil // coverage:ignore
			} // coverage:ignore
			return e.ingestRawFile(p, info.ModTime()) // coverage:ignore
		}) // coverage:ignore
	}
	info, err = os.Stat(realPath)
	if err != nil { // coverage:ignore
		return err // coverage:ignore
	} // coverage:ignore
	return e.ingestFile(path, info.ModTime())
}

// RenderTemplate delegates to internal/template.Render.
// Kept as a public alias for backward compatibility with existing callers.
func RenderTemplate(tmpl string, values map[string]any) (string, error) {
	return machetmpl.Render(tmpl, values)
}

// RenderTemplateWithFuncs delegates to internal/template.RenderWithFuncs.
func RenderTemplateWithFuncs(tmpl string, values map[string]any, extraFuncs template.FuncMap, cache *sync.Map) (string, error) {
	return machetmpl.RenderWithFuncs(tmpl, values, extraFuncs, cache)
}

// ReIngestFile re-ingests a single file, preserving the existing RootPath.
// Used by the live graph refresher to update stale nodes without a full walk.
// After re-ingestion, the store's file mtime is updated.
func (e *Engine) ReIngestFile(path string) error {
	absPath, err := filepath.Abs(path)
	if err != nil { // coverage:ignore
		return err // coverage:ignore
	} // coverage:ignore
	realPath, err := filepath.EvalSymlinks(absPath)
	if err != nil { // coverage:ignore
		realPath = absPath // coverage:ignore
	} // coverage:ignore

	info, err := os.Stat(realPath)
	if err != nil { // coverage:ignore
		return err // coverage:ignore
	} // coverage:ignore

	// Re-ingest the single file using the existing schema and store
	if err := e.ingestFile(realPath, info.ModTime()); err != nil { // coverage:ignore
		return err // coverage:ignore
	} // coverage:ignore

	// Update the tracked mtime in the store
	if ms, ok := e.Store.(*graph.MemoryStore); ok {
		ms.RecordFileMtime(realPath, info.ModTime())
	}

	return nil
}

// PrintRoutingSummary outputs a summary of files routed to _project_files/.
func (e *Engine) PrintRoutingSummary() { // coverage:ignore
	e.mu.Lock()         // coverage:ignore
	defer e.mu.Unlock() // coverage:ignore

	if len(e.routedFiles) > 0 { // coverage:ignore
		log.Printf("Routing summary:")           // coverage:ignore
		for lang, count := range e.routedFiles { // coverage:ignore
			log.Printf("  %s: %d files routed to _project_files/", lang, count) // coverage:ignore
		} // coverage:ignore
	} // coverage:ignore
}
