package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/template"
	"time"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/treesitter/elixir"
	sitter "github.com/smacker/go-tree-sitter"
	treec "github.com/smacker/go-tree-sitter/c"
	"github.com/smacker/go-tree-sitter/cpp"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/hcl"
	"github.com/smacker/go-tree-sitter/java"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/kotlin"
	"github.com/smacker/go-tree-sitter/php"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/ruby"
	"github.com/smacker/go-tree-sitter/rust"
	"github.com/smacker/go-tree-sitter/scala"
	"github.com/smacker/go-tree-sitter/sql"
	"github.com/smacker/go-tree-sitter/swift"
	"github.com/smacker/go-tree-sitter/toml"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
	"github.com/smacker/go-tree-sitter/yaml"
)

const inlineThreshold = 4096

// binarySniffSize is the number of bytes read from the start of a file
// to detect binary content (same heuristic as git).
const binarySniffSize = 512

// IngestionTarget combines Graph reading with writing capabilities.
type IngestionTarget interface {
	graph.Graph
	AddNode(n *graph.Node)
	AddRoot(n *graph.Node)
	AddRef(token, nodeID string) error
	AddDef(token, dirID string) error
	DeleteFileNodes(filePath string)
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

// --- Parallel ingestion types ---

// recordJob is sent from the SQLite reader to worker goroutines.
type recordJob struct {
	recordID string
	raw      string
}

// recordResult is the output from a worker: all nodes for one record.
type recordResult struct {
	nodes       []*graph.Node
	parentLinks []parentLink
	refLinks    []refLink
	err         error
}

type parentLink struct {
	childID  string
	parentID string
}

type refLink struct {
	token  string
	nodeID string
}

// --- Parallel tree-sitter ingestion types ---

// treeSitterJob represents a source file to parse with tree-sitter.
type treeSitterJob struct {
	path     string
	lang     *sitter.Language
	langName string
	modTime  time.Time
}

// parsedTreeSitterFile is the result of parallel tree-sitter parsing.
// Contains the pre-parsed AST and file content, ready for sequential processNode.
type parsedTreeSitterFile struct {
	job      treeSitterJob
	realPath string
	content  []byte
	tree     *sitter.Tree
	context  []byte // extracted imports/globals context
	parseErr error  // non-nil if tree-sitter parsing failed
	readErr  error  // non-nil if file read failed
}

// langForExt returns the tree-sitter language and name for a file extension.
// Returns nil, "" for unsupported extensions.
func langForExt(ext string) (*sitter.Language, string) {
	switch ext {
	case ".go":
		return golang.GetLanguage(), "go"
	case ".py":
		return python.GetLanguage(), "python"
	case ".js":
		return javascript.GetLanguage(), "javascript"
	case ".ts", ".tsx":
		return typescript.GetLanguage(), "typescript"
	case ".sql":
		return sql.GetLanguage(), "sql"
	case ".tf", ".hcl":
		return hcl.GetLanguage(), "hcl"
	case ".yaml", ".yml":
		return yaml.GetLanguage(), "yaml"
	case ".rs":
		return rust.GetLanguage(), "rust"
	case ".toml":
		return toml.GetLanguage(), "toml"
	case ".ex", ".exs":
		return elixir.GetLanguage(), "elixir"
	case ".java":
		return java.GetLanguage(), "java"
	case ".c", ".h":
		return treec.GetLanguage(), "c"
	case ".cpp", ".cc", ".cxx", ".hpp", ".hxx", ".hh":
		return cpp.GetLanguage(), "cpp"
	case ".rb":
		return ruby.GetLanguage(), "ruby"
	case ".php":
		return php.GetLanguage(), "php"
	case ".kt", ".kts":
		return kotlin.GetLanguage(), "kotlin"
	case ".swift":
		return swift.GetLanguage(), "swift"
	case ".scala", ".sc":
		return scala.GetLanguage(), "scala"
	default:
		return nil, ""
	}
}

// isBinaryFile returns true if the file appears to contain binary content.
// Uses the same heuristic as git: if the first 512 bytes contain a null byte,
// the file is binary. SQLite files (.db) are handled before this is called.
// MaxIngestFileSize is the largest file we'll read into memory during
// ingestion or schema inference. Files above this are silently skipped.
// Set to 0 to disable the size limit. Configurable via --max-file-size.
var MaxIngestFileSize int64 = 100 << 20 // 100 MB

// ParseSize parses a human-readable size string (e.g. "100MB", "1GB", "0").
// Returns bytes. Supported suffixes: KB, MB, GB (case-insensitive).
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "0" {
		return 0, nil
	}
	upper := strings.ToUpper(s)
	var multiplier int64 = 1
	numStr := s
	switch {
	case strings.HasSuffix(upper, "GB"):
		multiplier = 1 << 30
		numStr = s[:len(s)-2]
	case strings.HasSuffix(upper, "MB"):
		multiplier = 1 << 20
		numStr = s[:len(s)-2]
	case strings.HasSuffix(upper, "KB"):
		multiplier = 1 << 10
		numStr = s[:len(s)-2]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(numStr), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}
	return n * multiplier, nil
}

// skipExts are file extensions that are always skipped during directory walks.
var skipExts = map[string]bool{
	".o": true, ".a": true,
	".db": true, ".sqlite": true, ".sqlite3": true,
	".exe": true, ".dll": true, ".so": true, ".dylib": true,
	".zip": true, ".tar": true, ".gz": true, ".bz2": true, ".xz": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".ico": true,
	".woff": true, ".woff2": true, ".ttf": true, ".eot": true,
	".pdf": true, ".mp3": true, ".mp4": true, ".wav": true,
}

// ShouldSkipDir returns true for hidden dirs and common build artifact directories.
func ShouldSkipDir(base string) bool {
	if strings.HasPrefix(base, ".") {
		return true
	}
	switch base {
	case "node_modules", "target", "dist", "build", "__pycache__":
		return true
	}
	return false
}

// ShouldSkipFile returns true if the file should not be ingested.
// Checks extension blocklist, size limit, and binary content.
func ShouldSkipFile(path string, size int64) bool {
	ext := strings.ToLower(filepath.Ext(path))
	if skipExts[ext] {
		return true
	}
	if MaxIngestFileSize > 0 && size > MaxIngestFileSize {
		return true
	}
	return false
}

// ensureFile returns an error if path does not exist or is a directory.
func ensureFile(path, kind string) (os.FileInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory, not %s", path, kind)
	}
	return info, nil
}

func isBinaryFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, binarySniffSize)
	n, err := f.Read(buf)
	if err != nil && err != io.EOF {
		return false
	}
	return bytes.ContainsRune(buf[:n], 0)
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

// SetFileIndex sets a cached file index for incremental re-ingestion.
// Files matching (path, mtime, size) will be skipped during ingestion.
func (e *Engine) SetFileIndex(index map[string]FileIndexEntry) {
	e.fileIndex = index
}

// SchemaUsesTreeSitter returns true if the schema's selectors are tree-sitter
// S-expressions rather than JSONPath. S-expressions always start with '('.
func SchemaUsesTreeSitter(schema *api.Topology) bool {
	return hasTreeSitterSelectors(schema.Nodes)
}

// hasTreeSitterSelectors recursively checks for tree-sitter S-expression selectors.
func hasTreeSitterSelectors(nodes []api.Node) bool {
	for _, n := range nodes {
		sel := strings.TrimSpace(n.Selector)
		if len(sel) > 0 && sel[0] == '(' {
			return true
		}
		if hasTreeSitterSelectors(n.Children) {
			return true
		}
	}
	return false
}

// filterNodesByLanguage returns nodes that match the given language.
// Nodes match if:
// - Their Language field equals langName (FCA-generated nodes with language tags)
// - Their Name equals langName (namespace nodes from multi-language inference)
// - Their Language field is empty (manual schemas, tests, language-agnostic nodes)
func filterNodesByLanguage(nodes []api.Node, langName string) []api.Node {
	var result []api.Node
	for _, node := range nodes {
		if node.Language == langName || node.Name == langName || node.Language == "" {
			result = append(result, node)
		}
	}
	return result
}

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
	if err != nil {
		return err
	}
	realPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		realPath = absPath
	}
	e.RootPath = realPath

	info, err := os.Stat(realPath)
	if err != nil {
		return err
	}

	if info.IsDir() {
		// Load .gitignore patterns when enabled (default: true).
		if e.RespectGitignore {
			e.gitignore = loadGitignore(realPath)
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

		return filepath.WalkDir(realPath, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if p != realPath && ShouldSkipDir(d.Name()) {
					return filepath.SkipDir
				}
				// Check gitignore for directories
				if e.gitignore != nil && p != realPath {
					rel, relErr := filepath.Rel(realPath, p)
					if relErr == nil {
						rel = filepath.ToSlash(rel)
						if e.gitignore.Match(rel, true) {
							return filepath.SkipDir
						}
					}
				}
				return nil
			}
			// Check gitignore for files
			if e.gitignore != nil {
				rel, relErr := filepath.Rel(realPath, p)
				if relErr == nil {
					rel = filepath.ToSlash(rel)
					if e.gitignore.Match(rel, false) {
						return nil
					}
				}
			}
			// Skip symlinks to directories (e.g., kodata/templates -> ../templates)
			// WalkDir doesn't follow symlinks, so d.IsDir() is false for them,
			// but os.ReadFile will follow and fail with "is a directory".
			if d.Type()&os.ModeSymlink != 0 {
				target, err := os.Stat(p)
				if err == nil && target.IsDir() {
					return nil
				}
			}
			// Determine if we should parse or treat as raw based on schema type
			ext := filepath.Ext(p)
			info, err := d.Info()
			if err != nil {
				return err
			}
			if ShouldSkipFile(p, info.Size()) {
				return nil
			}
			shouldParse := false
			switch ext {
			case ".json", ".db":
				shouldParse = true
			}

			if shouldParse {
				return e.ingestFile(p, info.ModTime())
			}
			// Skip binary files (executables, object files, images, etc.)
			if isBinaryFile(p) {
				return nil
			}
			return e.ingestRawFile(p, info.ModTime())
		})
	}
	info, err = os.Stat(realPath)
	if err != nil {
		return err
	}
	return e.ingestFile(path, info.ModTime())
}

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
	for i := 0; i < numWorkers; i++ {
		workerWg.Add(1)
		go func() {
			defer workerWg.Done()
			parser := sitter.NewParser()
			for job := range jobs {
				result := parsedTreeSitterFile{job: job}
				absPath, err := filepath.Abs(job.path)
				if err != nil {
					result.readErr = err
					parsed <- result
					continue
				}
				result.realPath, err = filepath.EvalSymlinks(absPath)
				if err != nil {
					result.realPath = absPath
				}

				result.content, err = os.ReadFile(result.realPath)
				if err != nil {
					result.readErr = err
					parsed <- result
					continue
				}

				parser.SetLanguage(job.lang)
				tree, err := parser.ParseCtx(context.Background(), nil, result.content)
				if err != nil {
					result.parseErr = err
				} else {
					result.tree = tree
					// Extract context (imports, globals) — CPU-bound query execution.
					if ctxBytes, err := e.sitterWalker.ExtractContext(
						tree.RootNode(), result.content, job.lang, job.langName,
					); err == nil {
						result.context = ctxBytes
					}
				}
				parsed <- result
			}
		}()
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
				return err
			}
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
				target, err := os.Stat(p)
				if err == nil && target.IsDir() {
					return nil
				}
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
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
					lookupPath := p
					if resolved, err := filepath.EvalSymlinks(p); err == nil {
						lookupPath = resolved
					}
					if entry, ok := e.fileIndex[lookupPath]; ok {
						if entry.ModTime.Equal(info.ModTime()) && entry.Size == info.Size() {
							return nil // unchanged, skip re-parsing
						}
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
	for _, result := range results {
		processed++
		if processed%1000 == 0 {
			log.Printf("Ingested %d/%d files...", processed, fileCount.Load())
		}

		if result.readErr != nil {
			if firstErr == nil {
				firstErr = result.readErr
			}
			continue
		}

		if result.parseErr != nil {
			log.Printf("ingest: parse failed for %s (using raw fallback): %v", result.job.path, result.parseErr)
			// Fallback: ingest as broken file — use path hash to avoid ID collisions
			// when two broken files share the same basename in different directories.
			pathForID := result.realPath
			if pathForID == "" {
				pathForID = result.job.path
			}
			sum := sha256.Sum256([]byte(pathForID))
			fallbackID := "BROKEN_" + hex.EncodeToString(sum[:8])
			fileNode := &graph.Node{
				ID:      fallbackID,
				Mode:    0o444,
				ModTime: result.job.modTime,
				Data:    result.content,
				Origin: &graph.SourceOrigin{
					FilePath:  result.realPath,
					StartByte: 0,
					EndByte:   uint32(len(result.content)),
				},
			}
			e.Store.AddNode(fileNode)
			e.Store.AddRoot(fileNode)
			continue
		}

		// Apply parsed tree using processNode (sequential, touches shared state).
		bt := &bufferingTarget{IngestionTarget: e.Store}
		walker := e.sitterWalker
		root := SitterRoot{
			Node:     result.tree.RootNode(),
			FileRoot: result.tree.RootNode(),
			Source:   result.content,
			Lang:     result.job.lang,
			LangName: result.job.langName,
		}
		sourceFile := filepath.Base(result.job.path)
		applicableNodes := filterNodesByLanguage(e.Schema.Nodes, result.job.langName)

		// No applicable schema nodes for this language — route to _project_files/.
		if len(applicableNodes) == 0 {
			if err := e.ingestRawFileUnder(result.job.path, "_project_files", result.job.modTime); err != nil {
				if firstErr == nil {
					firstErr = err
				}
			}
			continue
		}

		// Extract file-level address refs once (e.g., HCL variable declarations).
		var fileAddrRefs []string
		if addrRefs, err := walker.ExtractAddressRefs(result.tree.RootNode(), result.content, result.job.lang, result.job.langName); err == nil {
			fileAddrRefs = addrRefs
		}

		var processErr error
		for _, nodeSchema := range applicableNodes {
			if err := e.processNode(nodeSchema, walker, root, "", sourceFile, result.realPath, result.job.modTime, bt, result.context, fileAddrRefs); err != nil {
				if strings.Contains(err.Error(), "invalid query") {
					e.mu.Lock()
					e.routedFiles[result.job.langName]++
					e.mu.Unlock()
					processErr = e.ingestRawFileUnder(result.job.path, "_project_files", result.job.modTime)
					break
				}
				processErr = fmt.Errorf("failed to process schema node %s: %w", nodeSchema.Name, err)
				break
			}
		}

		if processErr != nil {
			if firstErr == nil {
				firstErr = processErr
			}
			continue
		}

		// No nodes produced — schema selectors didn't match anything in this
		// language. Route to _project_files/ so the file isn't silently lost.
		if len(bt.bufferedNodes) == 0 {
			if err := e.ingestRawFileUnder(result.job.path, "_project_files", result.job.modTime); err != nil {
				if firstErr == nil {
					firstErr = err
				}
			}
			continue
		}

		// Atomic swap of file nodes.
		if ms, ok := e.Store.(*graph.MemoryStore); ok {
			ms.ReplaceFileNodes(result.realPath, bt.bufferedNodes)
		} else {
			e.Store.DeleteFileNodes(result.realPath)
			for _, n := range bt.bufferedNodes {
				e.Store.AddNode(n)
			}
		}

		// Record file metadata for incremental re-ingestion.
		if sw, ok := e.Store.(*SQLiteWriter); ok {
			info, err := os.Stat(result.realPath)
			if err == nil {
				sw.RecordFile(result.realPath, info.ModTime(), info.Size())
			}
		}
	}

	// Wait for walk to complete.
	<-doneCh
	if walkErr != nil {
		return walkErr
	}

	// Process raw (non-tree-sitter) files sequentially (cheap, no parsing).
	for _, rf := range rawFiles {
		if err := e.ingestRawFileUnder(rf.path, "_project_files", rf.modTime); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	if fileCount.Load() > 0 {
		log.Printf("Ingested %d source files total (%d workers).", processed, numWorkers)
	}

	return firstErr
}

func (e *Engine) ingestFile(path string, modTime time.Time) error {
	ext := filepath.Ext(path)

	switch ext {
	case ".db":
		return e.ingestSQLiteStreaming(path)
	case ".json":
		return e.ingestJSON(path, modTime)
	default:
		if lang, langName := langForExt(ext); lang != nil {
			return e.ingestTreeSitter(path, lang, langName, modTime)
		}
		if isBinaryFile(path) {
			return nil
		}
		return e.ingestRawFile(path, modTime)
	}
}

func (e *Engine) ingestJSON(path string, modTime time.Time) error {
	if _, err := ensureFile(path, "a JSON file"); err != nil {
		return err
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var data any
	if err := json.Unmarshal(content, &data); err != nil {
		return fmt.Errorf("failed to parse json %s: %w", path, err)
	}

	// Clear old nodes from this file (if any)
	absPath, _ := filepath.Abs(path)
	realPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		realPath = absPath
	}
	e.Store.DeleteFileNodes(realPath)

	walker := NewJsonWalker()
	for _, nodeSchema := range e.Schema.Nodes {
		if err := e.processNode(nodeSchema, walker, data, "", "", "", modTime, e.Store, nil, nil); err != nil {
			return fmt.Errorf("failed to process schema node %s: %w", nodeSchema.Name, err)
		}
	}
	return nil
}

// bufferingTarget buffers file nodes for atomic replacement while passing
// directory updates through immediately.
type bufferingTarget struct {
	IngestionTarget
	bufferedNodes []*graph.Node
}

func (b *bufferingTarget) AddNode(n *graph.Node) {
	if n.Mode.IsDir() {
		b.IngestionTarget.AddNode(n)
	} else {
		b.bufferedNodes = append(b.bufferedNodes, n)
	}
}

func (b *bufferingTarget) AddDef(token, dirID string) error {
	return b.IngestionTarget.AddDef(token, dirID)
}

func (e *Engine) ingestTreeSitter(path string, lang *sitter.Language, langName string, modTime time.Time) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	realPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		realPath = absPath
	}

	if _, err := ensureFile(realPath, "a source file"); err != nil {
		return err
	}

	content, err := os.ReadFile(realPath)
	if err != nil {
		return err
	}

	// ReplaceFileNodes handles deletion + addition atomically.
	bt := &bufferingTarget{IngestionTarget: e.Store}

	parser := sitter.NewParser()
	parser.SetLanguage(lang)
	tree, err := parser.ParseCtx(context.Background(), nil, content)
	if err != nil {
		log.Printf("ingest: parse failed for %s (using raw fallback): %v", path, err)
	}

	if err == nil {
		// Use the shared walker if available (set during Ingest), otherwise
		// create a temporary one (e.g., during ReIngestFile).
		walker := e.sitterWalker
		if walker == nil {
			walker = NewSitterWalker()
			defer walker.Close()
		}
		root := SitterRoot{Node: tree.RootNode(), FileRoot: tree.RootNode(), Source: content, Lang: lang, LangName: langName}
		sourceFile := filepath.Base(path)

		// Extract context (imports, globals) ONCE per file — shared across all constructs.
		// This avoids N duplicate allocations where N = number of constructs in the file.
		var fileContext []byte
		if ctxBytes, err := walker.ExtractContext(tree.RootNode(), content, lang, langName); err == nil {
			fileContext = ctxBytes
		}

		// Extract file-level address refs once (e.g., HCL variable declarations).
		var fileAddrRefs []string
		if addrRefs, err := walker.ExtractAddressRefs(tree.RootNode(), content, lang, langName); err == nil {
			fileAddrRefs = addrRefs
		}

		// Filter schema nodes by language to prevent cross-language query errors
		applicableNodes := filterNodesByLanguage(e.Schema.Nodes, langName)

		for _, nodeSchema := range applicableNodes {
			if err := e.processNode(nodeSchema, walker, root, "", sourceFile, realPath, modTime, bt, fileContext, fileAddrRefs); err != nil {
				// Tree-sitter query compilation fails when a schema selector
				// uses node types from a different language (e.g. Go's
				// "function_declaration" applied to a Python file). This is
				// expected when FCA infers a schema from mixed-language dirs.
				// Route to _project_files/ so the content is still accessible.
				if strings.Contains(err.Error(), "invalid query") {
					e.mu.Lock()
					e.routedFiles[langName]++
					e.mu.Unlock()
					return e.ingestRawFileUnder(path, "_project_files", modTime)
				}
				return fmt.Errorf("failed to process schema node %s: %w", nodeSchema.Name, err)
			}
		}
	}

	if err != nil {
		// Fallback logic
		baseName := filepath.Base(path)
		fallbackID := "BROKEN_" + baseName

		fileNode := &graph.Node{
			ID:      fallbackID,
			Mode:    0o444,
			ModTime: modTime,
			Data:    content,
			Origin: &graph.SourceOrigin{
				FilePath:  realPath,
				StartByte: 0,
				EndByte:   uint32(len(content)),
			},
		}
		bt.AddNode(fileNode)
		e.Store.AddRoot(fileNode)
	}

	// atomic swap
	if ms, ok := e.Store.(*graph.MemoryStore); ok {
		ms.ReplaceFileNodes(realPath, bt.bufferedNodes)
	} else {
		// Fallback for non-MemoryStore (shouldn't happen in write-back)
		e.Store.DeleteFileNodes(realPath)
		for _, n := range bt.bufferedNodes {
			e.Store.AddNode(n)
		}
	}

	return nil
}

func (e *Engine) ingestRawFile(path string, modTime time.Time) error {
	return e.ingestRawFileUnder(path, "", modTime)
}

func (e *Engine) ingestRawFileUnder(path, prefix string, modTime time.Time) error {
	rel, err := filepath.Rel(e.RootPath, path)
	if err != nil {
		return err
	}
	rel = filepath.ToSlash(rel)
	parts := strings.Split(rel, "/")

	// When a prefix is set, lazily create the prefix root node on first use.
	parentID := prefix
	if prefix != "" {
		if _, err := e.Store.GetNode(prefix); err != nil {
			pfNode := &graph.Node{ID: prefix, Mode: os.ModeDir | 0o555}
			e.Store.AddNode(pfNode)
			e.Store.AddRoot(pfNode)
		}
	}

	// 1. Create/Ensure intermediate directories
	for i := 0; i < len(parts)-1; i++ {
		part := parts[i]
		var currentID string
		if parentID != "" {
			currentID = parentID + "/" + part
		} else {
			currentID = part
		}

		if _, err := e.Store.GetNode(currentID); err != nil {
			// Create directory node
			node := &graph.Node{
				ID:   currentID,
				Mode: os.ModeDir | 0o555,
			}
			e.Store.AddNode(node)

			// Link to parent
			if parentID == "" {
				e.Store.AddRoot(node)
			} else {
				parent, err := e.Store.GetNode(parentID)
				if err == nil {
					if e.childSeen[parentID] == nil {
						e.childSeen[parentID] = make(map[string]bool, len(parent.Children))
						for _, c := range parent.Children {
							e.childSeen[parentID][c] = true
						}
					}
					if !e.childSeen[parentID][currentID] {
						e.childSeen[parentID][currentID] = true
						parent.Children = append(parent.Children, currentID)
						e.Store.AddNode(parent)
					}
				}
			}
		}
		parentID = currentID
	}

	// 2. Create file node
	var fileID string
	if prefix != "" {
		fileID = prefix + "/" + rel
	} else {
		fileID = rel
	}

	info, err := ensureFile(path, "a raw file")
	if err != nil {
		return err
	}
	if ShouldSkipFile(path, info.Size()) {
		return nil
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	// Use time.Now() to force NFS cache invalidation
	// modTime := time.Now()
	// Replaced with actual modTime passed from caller

	absPath, _ := filepath.Abs(path)
	e.Store.DeleteFileNodes(absPath)

	fileNode := &graph.Node{
		ID:      fileID,
		Mode:    0o444,
		ModTime: modTime,
		Data:    content,
		Origin: &graph.SourceOrigin{
			FilePath:  absPath,
			StartByte: 0,
			EndByte:   uint32(len(content)),
		},
	}
	e.Store.AddNode(fileNode)

	// Link to parent
	if parentID == "" {
		e.Store.AddRoot(fileNode)
	} else {
		parent, err := e.Store.GetNode(parentID)
		if err == nil {
			parent.Children = append(parent.Children, fileID)
			e.Store.AddNode(parent)
		}
	}

	return nil
}

// ingestSQLiteStreaming processes a SQLite database using a parallel worker pool.
// Reader goroutine streams rows, workers parse JSON + render templates,
// collector applies nodes to the store. Saturates all CPU cores.
func (e *Engine) ingestSQLiteStreaming(dbPath string) error {
	// Pre-create root directory nodes from schema
	for _, nodeSchema := range e.Schema.Nodes {
		rootNode := &graph.Node{
			ID:   nodeSchema.Name,
			Mode: os.ModeDir | 0o555,
		}
		e.Store.AddNode(rootNode)
		e.Store.AddRoot(rootNode)
	}

	numWorkers := runtime.NumCPU()
	jobs := make(chan recordJob, numWorkers*2)
	results := make(chan recordResult, numWorkers*2)

	// Workers: parse JSON, render templates, build nodes.
	// DiagramFuncMap is safe for concurrent reads (built once, then shared).
	diagramFuncs := e.DiagramFuncMap()
	diagramCache := &e.diagramTmplCache

	var workerWg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		workerWg.Add(1)
		go func() {
			defer workerWg.Done()
			w := NewJsonWalker()
			for job := range jobs {
				results <- processRecord(e.Schema, w, dbPath, job, diagramFuncs, diagramCache)
			}
		}()
	}

	// Collector: apply nodes to store (single goroutine, no lock contention).
	// Handles dedup for shared directory nodes (e.g. year dirs from temporal sharding)
	// and parent-child links.
	var collectErr error
	var collectWg sync.WaitGroup
	collectWg.Add(1)
	go func() {
		defer collectWg.Done()
		parentChildSeen := make(map[string]map[string]bool)
		count := 0
		for res := range results {
			count++
			if count%50000 == 0 {
				log.Printf("Processed %d records...", count)
			}
			if res.err != nil {
				if collectErr == nil {
					collectErr = res.err
				}
				continue
			}
			for _, node := range res.nodes {
				// For directory nodes, only create if it doesn't exist yet.
				// Multiple workers may produce the same intermediate dir (e.g. "by-cve/2024").
				// Children are managed exclusively via parentLinks below.
				if node.Mode.IsDir() {
					if _, err := e.Store.GetNode(node.ID); err != nil {
						e.Store.AddNode(node)
					}
				} else {
					e.Store.AddNode(node)
				}
			}
			for _, link := range res.parentLinks {
				if parentChildSeen[link.parentID] == nil {
					parentChildSeen[link.parentID] = make(map[string]bool)
				}
				if !parentChildSeen[link.parentID][link.childID] {
					parentChildSeen[link.parentID][link.childID] = true
					parent, err := e.Store.GetNode(link.parentID)
					if err == nil {
						parent.Children = append(parent.Children, link.childID)
					}
				}
			}
			for _, ref := range res.refLinks {
				if err := e.Store.AddRef(ref.token, ref.nodeID); err != nil {
					if collectErr == nil {
						collectErr = fmt.Errorf("add ref %s -> %s: %w", ref.token, ref.nodeID, err)
					}
				}
			}
		}
		log.Printf("Processed %d records total.", count)
	}()

	// Reader: stream raw rows from SQLite (I/O bound, single goroutine)
	readErr := StreamSQLiteRaw(dbPath, func(id, raw string) error {
		jobs <- recordJob{recordID: id, raw: raw}
		return nil
	})

	close(jobs)     // signal workers: no more jobs
	workerWg.Wait() // wait for all workers to finish
	close(results)  // signal collector: no more results
	collectWg.Wait()

	if collectErr != nil {
		return collectErr
	}
	return readErr
}

// processRecord is a pure function — parses one SQLite record through the schema
// and returns all nodes to create, without touching the store.
//
// extraFuncs and tmplCache enable template functions like {{diagram}} in content
// templates. When non-nil, content rendering uses RenderTemplateWithFuncs with
// these extras merged in. When nil, falls back to RenderTemplate (base funcs only).
func processRecord(schema *api.Topology, walker Walker, dbPath string, job recordJob, extraFuncs template.FuncMap, tmplCache *sync.Map) recordResult {
	var parsed any
	if err := json.Unmarshal([]byte(job.raw), &parsed); err != nil {
		return recordResult{err: fmt.Errorf("parse record %s: %w", job.recordID, err)}
	}

	wrapper := []any{parsed}
	var result recordResult

	for _, nodeSchema := range schema.Nodes {
		for _, childSchema := range nodeSchema.Children {
			collectNodes(&result, childSchema, walker, wrapper, nodeSchema.Name, dbPath, job.recordID, extraFuncs, tmplCache)
			if result.err != nil {
				return result
			}
		}
	}

	return result
}

// collectNodes is the pure equivalent of processNode — builds node lists
// without any store access. Safe to call from multiple goroutines.
//
// extraFuncs/tmplCache are threaded through for content template rendering
// (e.g., {{diagram}}). When nil, uses base RenderTemplate.
func collectNodes(result *recordResult, schema api.Node, walker Walker, ctx any, parentPath, dbPath, recordID string, extraFuncs template.FuncMap, tmplCache *sync.Map) {
	matches, err := walker.Query(ctx, schema.Selector)
	if err != nil {
		result.err = fmt.Errorf("query failed for %s: %w", schema.Name, err)
		return
	}

	for _, match := range matches {
		name, err := RenderTemplate(schema.Name, match.Values())
		if err != nil {
			result.err = fmt.Errorf("failed to render name %s: %w", schema.Name, err)
			return
		}

		currentPath := filepath.Join(parentPath, name)
		id := toNodeID(currentPath)

		node := &graph.Node{
			ID:      id,
			Mode:    os.ModeDir | 0o555,
			ModTime: time.Unix(0, 0),
		}

		// Recurse children
		nextCtx := match.Context()
		if nextCtx != nil {
			for _, childSchema := range schema.Children {
				collectNodes(result, childSchema, walker, nextCtx, currentPath, dbPath, recordID, extraFuncs, tmplCache)
				if result.err != nil {
					return
				}
			}
		}

		// Process files
		for _, fileSchema := range schema.Files {
			fileName, err := RenderTemplate(fileSchema.Name, match.Values())
			if err != nil {
				log.Printf("collectNodes: skip file name render %q: %v", fileSchema.Name, err)
				continue
			}
			filePath := filepath.Join(currentPath, fileName)
			fileId := toNodeID(filePath)

			var content string
			if len(extraFuncs) > 0 && tmplCache != nil {
				content, err = RenderTemplateWithFuncs(fileSchema.ContentTemplate, match.Values(), extraFuncs, tmplCache)
			} else {
				content, err = RenderTemplate(fileSchema.ContentTemplate, match.Values())
			}
			if err != nil {
				log.Printf("collectNodes: skip file content render %q: %v", fileId, err)
				continue
			}

			fileNode := &graph.Node{
				ID:      fileId,
				Mode:    0o444,
				ModTime: time.Unix(0, 0),
			}

			// Inline small content, lazy-resolve large content from SQLite
			if len(content) > inlineThreshold {
				fileNode.Ref = &graph.ContentRef{
					DBPath:     dbPath,
					RecordID:   recordID,
					Template:   fileSchema.ContentTemplate,
					ContentLen: int64(len(content)),
				}
			} else {
				fileNode.Data = []byte(content)
			}

			result.nodes = append(result.nodes, fileNode)
			node.Children = append(node.Children, fileId)
		}

		result.nodes = append(result.nodes, node)

		// Collect schema-declared refs (cross-reference tokens for callers/)
		for _, refTmpl := range schema.Refs {
			token, err := RenderTemplate(refTmpl, match.Values())
			if err != nil {
				result.err = fmt.Errorf("failed to render ref %s: %w", refTmpl, err)
				return
			}
			if token != "" {
				result.refLinks = append(result.refLinks, refLink{token: token, nodeID: id})
			}
		}

		// Link to parent (collector will apply this)
		parentID := toNodeID(parentPath)
		result.parentLinks = append(result.parentLinks, parentLink{childID: id, parentID: parentID})
	}
}

// toNodeID converts a filesystem path to a graph node ID by normalizing
// separators and stripping the leading slash.
func toNodeID(p string) string {
	return strings.TrimPrefix(filepath.ToSlash(p), "/")
}

// dedupSuffix returns a ".from_<sanitized>" suffix derived from the source filename.
// Dots in the filename are replaced with underscores to avoid path separator confusion.
// e.g., "a.go" -> ".from_a_go"
func dedupSuffix(sourceFile string) string {
	sanitized := strings.ReplaceAll(sourceFile, ".", "_")
	return ".from_" + sanitized
}

func (e *Engine) processNode(schema api.Node, walker Walker, ctx any, parentPath, sourceFile, absSourceFile string, modTime time.Time, store IngestionTarget, fileContext []byte, fileAddressRefs []string) error {
	matches, err := walker.Query(ctx, schema.Selector)
	if err != nil {
		return fmt.Errorf("query failed for %s: %w", schema.Name, err)
	}

	for _, match := range matches {
		// Skip self-match if requested (e.g. for recursive schemas to avoid infinite loops)
		if schema.SkipSelfMatch {
			// Check for Tree-sitter node equality using byte ranges
			if parentRoot, ok := ctx.(SitterRoot); ok {
				if childCtx, ok := match.Context().(SitterRoot); ok {
					if parentRoot.Node.StartByte() == childCtx.Node.StartByte() &&
						parentRoot.Node.EndByte() == childCtx.Node.EndByte() &&
						parentRoot.Node.Type() == childCtx.Node.Type() {
						continue
					}
				}
			}
		}

		name, err := RenderTemplate(schema.Name, match.Values())
		if err != nil {
			return fmt.Errorf("failed to render name %s: %w", schema.Name, err)
		}

		// Normalize path
		currentPath := filepath.Join(parentPath, name)
		id := toNodeID(currentPath)

		// Dedup: when this node has files and a node with the same ID
		// already exists with file children (i.e., from a different source file),
		// append a source-file suffix to disambiguate.
		// This handles cases like multiple init() functions across Go files.
		if len(schema.Files) > 0 && sourceFile != "" {
			if existing, err := store.GetNode(id); err == nil && len(existing.Children) > 0 {
				suffix := dedupSuffix(sourceFile)
				name = name + suffix
				currentPath = filepath.Join(parentPath, name)
				id = toNodeID(currentPath)
			}
		}

		// Create/Update Node — preserve existing children when merging
		// multiple files into the same node (e.g. multiple .go files in one package).
		var existingChildren []string
		if existing, err := store.GetNode(id); err == nil {
			existingChildren = existing.Children
		}

		node := &graph.Node{
			ID:       id,
			Mode:     os.ModeDir | 0o555, // Read-only dir
			ModTime:  modTime,            // Propagate source file time
			Children: existingChildren,
		}

		// Store language name, package name, and register definition for callees/ resolution
		if _, ok := walker.(*SitterWalker); ok {
			if ctxAny := match.Context(); ctxAny != nil {
				if root, ok := ctxAny.(SitterRoot); ok && root.LangName != "" {
					if node.Properties == nil {
						node.Properties = make(map[string][]byte)
					}
					node.Properties["lang"] = []byte(root.LangName)

					// Extract Go package name for qualified def resolution
					if root.LangName == "go" && root.FileRoot != nil {
						if pkgName := extractGoPackageName(root.FileRoot, root.Source, root.Lang); pkgName != "" {
							node.Properties["pkg"] = []byte(pkgName)
						}
					}
				}
			}
		}
		store.AddNode(node)

		// Register definition: construct name → directory ID
		if len(schema.Files) > 0 {
			if err := store.AddDef(name, id); err != nil {
				return fmt.Errorf("add def %s -> %s: %w", name, id, err)
			}
			// Register qualified definition (package.name → directory ID)
			if node.Properties != nil {
				if pkg, ok := node.Properties["pkg"]; ok && len(pkg) > 0 {
					qualKey := string(pkg) + "." + name
					if err := store.AddDef(qualKey, id); err != nil {
						return fmt.Errorf("add qualified def %s -> %s: %w", qualKey, id, err)
					}
				}
			}
		}

		// Register schema-declared refs (cross-reference tokens for callers/)
		for _, refTmpl := range schema.Refs {
			token, err := RenderTemplate(refTmpl, match.Values())
			if err != nil {
				return fmt.Errorf("failed to render ref %s: %w", refTmpl, err)
			}
			if token != "" {
				if err := store.AddRef(token, id); err != nil {
					return fmt.Errorf("add ref %s -> %s: %w", token, id, err)
				}
			}
		}

		// Link to parent
		if parentPath == "" {
			store.AddRoot(node)
		} else {
			parentId := toNodeID(parentPath)
			parent, err := store.GetNode(parentId)
			if err == nil {
				if e.childSeen[parentId] == nil {
					e.childSeen[parentId] = make(map[string]bool, len(parent.Children))
					for _, c := range parent.Children {
						e.childSeen[parentId][c] = true
					}
				}
				if !e.childSeen[parentId][id] {
					e.childSeen[parentId][id] = true
					parent.Children = append(parent.Children, id)
					store.AddNode(parent)
				}
			}
		}

		// Recurse children
		nextCtx := match.Context()
		if nextCtx != nil {
			for _, childSchema := range schema.Children {
				if err := e.processNode(childSchema, walker, nextCtx, currentPath, sourceFile, absSourceFile, modTime, store, fileContext, fileAddressRefs); err != nil {
					return err
				}
			}
		}

		// Extract calls for this match (refs index)
		var calls []string
		if sw, ok := walker.(*SitterWalker); ok {
			if ctxAny := match.Context(); ctxAny != nil {
				if root, ok := ctxAny.(SitterRoot); ok {
					if c, err := sw.ExtractCalls(root.Node, root.Source, root.Lang, root.LangName); err == nil {
						calls = c
					}
					// Extract address-aware refs (env:, path:, url:) from the
					// match scope. These typed tokens bridge across languages
					// (e.g., Go os.Getenv calls within this function scope).
					if addrRefs, err := sw.ExtractAddressRefs(root.Node, root.Source, root.Lang, root.LangName); err == nil {
						calls = append(calls, addrRefs...)
					}
				}
			}
		}
		// Append file-level address refs (e.g., HCL variable declarations)
		// that weren't already found at the scope level. This avoids
		// duplicate refs when a Go function both calls os.Getenv and the
		// file-root also matches the same pattern.
		if len(fileAddressRefs) > 0 {
			scopeSeen := make(map[string]bool, len(calls))
			for _, c := range calls {
				scopeSeen[c] = true
			}
			for _, ref := range fileAddressRefs {
				if !scopeSeen[ref] {
					calls = append(calls, ref)
				}
			}
		}

		// Re-fetch current node (updated by recursion) — preserve Children + Properties
		var currentChildren []string
		var currentProps map[string][]byte
		if current, err := store.GetNode(id); err == nil {
			currentChildren = current.Children
			currentProps = current.Properties
		}

		// Pre-compute doc comments from backward scan (available to all file templates)
		docText, extStart, extEnd, hasScope := extractDocComments(match)

		node = &graph.Node{
			ID:         id,
			Mode:       os.ModeDir | 0o555, // Read-only dir
			ModTime:    modTime,            // Propagate source file time
			Children:   currentChildren,
			Context:    fileContext,
			Properties: currentProps,
		}

		// Set location property on directory node from source file's origin
		if hasScope && absSourceFile != "" {
			if root, ok := match.Context().(SitterRoot); ok {
				if node.Properties == nil {
					node.Properties = make(map[string][]byte)
				}
				relPath, err := filepath.Rel(e.RootPath, absSourceFile)
				if err == nil {
					startLine := byteOffsetToLine(root.Source, extStart)
					endLine := byteOffsetToLine(root.Source, extEnd)
					node.Properties["location"] = []byte(fmt.Sprintf("%s:%d:%d", relPath, startLine, endLine))
				}
			}
		}
		store.AddNode(node)

		for _, fileSchema := range schema.Files {
			fileName, err := RenderTemplate(fileSchema.Name, match.Values())
			if err != nil {
				log.Printf("processNode: skip file name render %q: %v", fileSchema.Name, err)
				continue
			}
			filePath := filepath.Join(currentPath, fileName)
			fileId := toNodeID(filePath)

			// Augment template values with doc comment text
			vals := match.Values()
			if docText != "" {
				vals["doc"] = docText
			}

			content, err := e.RenderContentTemplate(fileSchema.ContentTemplate, vals)
			if err != nil {
				log.Printf("processNode: skip file content render %q: %v", fileId, err)
				continue
			}

			// Skip empty optional files (e.g. "doc" when no doc comments exist)
			if content == "" && fileSchema.Name != "source" {
				continue
			}

			fileNode := &graph.Node{
				ID:      fileId,
				Mode:    0o444,
				ModTime: modTime,
				Data:    []byte(content),
			}

			// Extend source file content to include preceding doc comments
			if hasScope && docText != "" && fileSchema.Name == "source" {
				if root, ok := match.Context().(SitterRoot); ok {
					if extEnd <= uint32(len(root.Source)) {
						fileNode.Data = root.Source[extStart:extEnd]
					}
				}
			}

			// Set write-back origin from backward scan
			if hasScope && absSourceFile != "" {
				fileNode.Origin = &graph.SourceOrigin{
					FilePath:  absSourceFile,
					StartByte: extStart,
					EndByte:   extEnd,
				}
			} else if op, ok := match.(OriginProvider); ok && absSourceFile != "" {
				// Fallback for non-sitter matches
				if start, end, ok := op.CaptureOrigin("scope"); ok {
					fileNode.Origin = &graph.SourceOrigin{
						FilePath:  absSourceFile,
						StartByte: start,
						EndByte:   end,
					}
				}
			}

			store.AddNode(fileNode)
			node.Children = append(node.Children, fileId)
			store.AddNode(node)

			// Update Index — only for the source file to avoid duplicate refs
			if fileSchema.Name == "source" {
				for _, token := range calls {
					if err := store.AddRef(token, fileId); err != nil {
						return fmt.Errorf("add ref %s -> %s: %w", token, fileId, err)
					}
				}
			}
		}
	}
	return nil
}

// byteOffsetToLine converts a byte offset to a 1-based line number in content.
func byteOffsetToLine(content []byte, offset uint32) int {
	line := 1
	for i := 0; i < int(offset) && i < len(content); i++ {
		if content[i] == '\n' {
			line++
		}
	}
	return line
}

// extractDocComments walks backward from a tree-sitter @scope capture to find
// contiguous preceding comment nodes. Returns the doc comment text (just the
// comments) and the extended byte range for write-back origin tracking.
func extractDocComments(match Match) (docText string, startByte, endByte uint32, hasScope bool) {
	sm, ok := match.(interface{ GetCaptureNode(string) *sitter.Node })
	if !ok {
		return docText, startByte, endByte, hasScope
	}
	scopeNode := sm.GetCaptureNode("scope")
	if scopeNode == nil {
		return docText, startByte, endByte, hasScope
	}
	hasScope = true
	startByte = scopeNode.StartByte()
	endByte = scopeNode.EndByte()

	// Walk backward to find contiguous comment siblings
	n := scopeNode
	prev := n.PrevSibling()
	for prev != nil && prev.Type() == "comment" {
		// Check adjacency: <= 2 bytes gap (allow \n or \n\n)
		if int(n.StartByte())-int(prev.EndByte()) <= 2 {
			startByte = prev.StartByte()
			n = prev
			prev = prev.PrevSibling()
		} else {
			break
		}
	}

	// Extract doc comment text (just the comments, not the scope body)
	if startByte < scopeNode.StartByte() {
		if root, ok := match.Context().(SitterRoot); ok {
			if scopeNode.StartByte() <= uint32(len(root.Source)) {
				docText = strings.TrimRight(
					string(root.Source[startByte:scopeNode.StartByte()]),
					"\n\r\t ",
				)
			}
		}
	}
	return docText, startByte, endByte, hasScope
}

// --- Go package name extraction for qualified defs ---

var (
	goPackageQueryOnce sync.Once
	goPackageQueryObj  *sitter.Query
)

// extractGoPackageName uses tree-sitter to find the package name from a Go file root.
func extractGoPackageName(fileRoot *sitter.Node, source []byte, lang *sitter.Language) string {
	goPackageQueryOnce.Do(func() {
		goPackageQueryObj, _ = sitter.NewQuery([]byte(`(package_clause (package_identifier) @pkg)`), lang)
	})
	if goPackageQueryObj == nil {
		return ""
	}

	qc := sitter.NewQueryCursor()
	defer qc.Close()
	qc.Exec(goPackageQueryObj, fileRoot)

	m, ok := qc.NextMatch()
	if !ok || len(m.Captures) == 0 {
		return ""
	}

	c := m.Captures[0]
	start := c.Node.StartByte()
	end := c.Node.EndByte()
	if start < uint32(len(source)) && end <= uint32(len(source)) {
		return string(source[start:end])
	}
	return ""
}

// GetLanguage returns the tree-sitter language for a language name string.
// Returns nil for unsupported languages.
func GetLanguage(langName string) *sitter.Language {
	switch langName {
	case "go":
		return golang.GetLanguage()
	case "python":
		return python.GetLanguage()
	case "javascript":
		return javascript.GetLanguage()
	case "typescript":
		return typescript.GetLanguage()
	case "sql":
		return sql.GetLanguage()
	case "hcl":
		return hcl.GetLanguage()
	case "yaml":
		return yaml.GetLanguage()
	case "rust":
		return rust.GetLanguage()
	case "toml":
		return toml.GetLanguage()
	case "elixir":
		return elixir.GetLanguage()
	case "java":
		return java.GetLanguage()
	case "c":
		return treec.GetLanguage()
	case "cpp":
		return cpp.GetLanguage()
	case "ruby":
		return ruby.GetLanguage()
	case "php":
		return php.GetLanguage()
	case "kotlin":
		return kotlin.GetLanguage()
	case "swift":
		return swift.GetLanguage()
	case "scala":
		return scala.GetLanguage()
	default:
		return nil
	}
}

var tmplFuncs = template.FuncMap{
	"json": func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("<json error: %v>", err)
		}
		return string(b)
	},
	"first": func(v any) any {
		switch s := v.(type) {
		case []any:
			if len(s) > 0 {
				return s[0]
			}
		}
		return nil
	},
	// unquote strips Go string quotes: {{unquote .path}} → cobra from "cobra".
	// Tree-sitter captures of interpreted_string_literal include surrounding quotes.
	"unquote": func(s string) string {
		if u, err := strconv.Unquote(s); err == nil {
			return u
		}
		return s
	},
	// slice extracts a substring: {{slice .someField 4 8}} → characters [4:8].
	// Used for temporal sharding: {{slice .item.cve.id 4 8}} → "2024" from "CVE-2024-0001".
	"slice": func(s string, start, end int) string {
		if start < 0 {
			start = 0
		}
		if end > len(s) {
			end = len(s)
		}
		if start >= end {
			return ""
		}
		return s[start:end]
	},
}

// tmplCache stores parsed templates keyed by their source string.
// template.Template.Execute is safe for concurrent use (Go docs guarantee this),
// so a shared cache with sync.Map is correct. Each caller uses its own bytes.Buffer.
var tmplCache sync.Map // template string → *template.Template

// RenderTemplate renders a Go text/template with the standard mache template functions.
// Parsed templates are cached — repeated calls with the same template string skip parsing.
func RenderTemplate(tmpl string, values map[string]any) (string, error) {
	var t *template.Template
	if cached, ok := tmplCache.Load(tmpl); ok {
		t = cached.(*template.Template)
	} else {
		var err error
		t, err = template.New("").Funcs(tmplFuncs).Parse(tmpl)
		if err != nil {
			return "", err
		}
		tmplCache.Store(tmpl, t)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, values); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// RenderTemplateWithFuncs renders a Go text/template with the standard mache
// template functions plus additional per-engine functions (e.g., {{diagram}}).
// Templates are cached in the provided cache; the caller must ensure the
// extraFuncs map is stable for the cache's lifetime.
func RenderTemplateWithFuncs(tmpl string, values map[string]any, extraFuncs template.FuncMap, cache *sync.Map) (string, error) {
	var t *template.Template
	if cached, ok := cache.Load(tmpl); ok {
		t = cached.(*template.Template)
	} else {
		merged := make(template.FuncMap, len(tmplFuncs)+len(extraFuncs))
		for k, v := range tmplFuncs {
			merged[k] = v
		}
		for k, v := range extraFuncs {
			merged[k] = v
		}
		var err error
		t, err = template.New("").Funcs(merged).Parse(tmpl)
		if err != nil {
			return "", err
		}
		cache.Store(tmpl, t)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, values); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// refsProvider is the subset of stores that expose their refs map.
type refsProvider interface {
	RefsMap() map[string][]string
}

// ensureDiagramData lazily computes and caches the CommunityResult and refs.
// Safe for concurrent use; the computation runs at most once.
func (e *Engine) ensureDiagramData() {
	e.diagramOnce.Do(func() {
		rp, ok := e.Store.(refsProvider)
		if !ok {
			return
		}
		e.cachedRefs = rp.RefsMap()
		if len(e.cachedRefs) > 0 {
			e.cachedCommunities = graph.DetectCommunities(e.cachedRefs, 2)
		}
	})
}

// DiagramFuncMap returns a template.FuncMap containing the {{diagram "name"}}
// function. The returned FuncMap is built once via sync.Once and reused;
// the closure inside captures the Engine and lazily initializes community
// data on first call. Safe for concurrent use.
func (e *Engine) DiagramFuncMap() template.FuncMap {
	e.diagramFuncMapOnce.Do(func() {
		e.diagramFuncMap = template.FuncMap{
			"diagram": func(name string) string {
				e.ensureDiagramData()

				if e.cachedCommunities == nil || len(e.cachedCommunities.Communities) == 0 {
					return "%% diagram: no communities detected"
				}

				// Determine layout from schema diagrams map or default.
				layout := "TD"
				if e.Schema != nil && e.Schema.Diagrams != nil {
					if def, ok := e.Schema.Diagrams[name]; ok {
						layout = def.Layout
					} else if name != "system" {
						return fmt.Sprintf("%% diagram %q not defined", name)
					}
				}

				q := graph.ComputeQuotient(e.cachedCommunities, e.cachedRefs)
				return q.Mermaid(layout)
			},
		}
	})
	return e.diagramFuncMap
}

// RenderContentTemplate renders a content template with the standard mache
// functions plus the Engine's diagram function. This is the method that
// processNode and collectNodes should use for file content rendering.
func (e *Engine) RenderContentTemplate(tmpl string, values map[string]any) (string, error) {
	return RenderTemplateWithFuncs(tmpl, values, e.DiagramFuncMap(), &e.diagramTmplCache)
}

// ReIngestFile re-ingests a single file, preserving the existing RootPath.
// Used by the live graph refresher to update stale nodes without a full walk.
// After re-ingestion, the store's file mtime is updated.
func (e *Engine) ReIngestFile(path string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	realPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		realPath = absPath
	}

	info, err := os.Stat(realPath)
	if err != nil {
		return err
	}

	// Re-ingest the single file using the existing schema and store
	if err := e.ingestFile(realPath, info.ModTime()); err != nil {
		return err
	}

	// Update the tracked mtime in the store
	if ms, ok := e.Store.(*graph.MemoryStore); ok {
		ms.RecordFileMtime(realPath, info.ModTime())
	}

	return nil
}

// PrintRoutingSummary outputs a summary of files routed to _project_files/.
func (e *Engine) PrintRoutingSummary() {
	e.mu.Lock()
	defer e.mu.Unlock()

	if len(e.routedFiles) > 0 {
		log.Printf("Routing summary:")
		for lang, count := range e.routedFiles {
			log.Printf("  %s: %d files routed to _project_files/", lang, count)
		}
	}
}
