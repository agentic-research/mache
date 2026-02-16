package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/hcl"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/rust"
	"github.com/smacker/go-tree-sitter/sql"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
	"github.com/smacker/go-tree-sitter/yaml"
)

const inlineThreshold = 4096

// IngestionTarget combines Graph reading with writing capabilities.
type IngestionTarget interface {
	graph.Graph
	AddNode(n *graph.Node)
	AddRoot(n *graph.Node)
	AddRef(token, nodeID string) error
	DeleteFileNodes(filePath string)
}

// Engine drives the ingestion process.
type Engine struct {
	Schema     *api.Topology
	Store      IngestionTarget
	RootPath   string // absolute path to the root of the ingestion
	sourceFile string // absolute path, set during ingestTreeSitter for origin tracking
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
	err         error
}

type parentLink struct {
	childID  string
	parentID string
}

func NewEngine(schema *api.Topology, store IngestionTarget) *Engine {
	return &Engine{
		Schema: schema,
		Store:  store,
	}
}

// schemaUsesTreeSitter returns true if the schema's selectors are tree-sitter
// S-expressions rather than JSONPath. S-expressions always start with '('.
func schemaUsesTreeSitter(schema *api.Topology) bool {
	for _, n := range schema.Nodes {
		sel := strings.TrimSpace(n.Selector)
		if len(sel) > 0 && sel[0] == '(' {
			return true
		}
	}
	return false
}

// Ingest processes a file or directory.
func (e *Engine) Ingest(path string) error {
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
		// Determine which file types this schema can process.
		// Tree-sitter schemas operate on source code (.go, .py);
		// JSONPath schemas operate on data files (.json, .db).
		// Ingesting the wrong type is harmless but wastes time and
		// can produce confusing errors (e.g. S-expression as JSONPath).
		treeSitter := schemaUsesTreeSitter(e.Schema)

		return filepath.Walk(realPath, func(p string, d os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				// Skip hidden directories (.git, .mache, etc.) and build artifacts
				base := filepath.Base(p)
				if p != realPath {
					if len(base) > 0 && base[0] == '.' {
						return filepath.SkipDir
					}
					if base == "target" || base == "node_modules" || base == "dist" || base == "build" {
						return filepath.SkipDir
					}
				}
				return nil
			}
			// Determine if we should parse or treat as raw based on schema type
			ext := filepath.Ext(p)
			if ext == ".o" || ext == ".a" {
				return nil // Skip binary artifacts
			}
			shouldParse := false
			if treeSitter {
				switch ext {
				case ".go", ".py", ".js", ".ts", ".tsx", ".sql", ".rs", ".tf", ".hcl", ".yaml", ".yml":
					shouldParse = true
				}
			} else {
				switch ext {
				case ".json", ".db":
					shouldParse = true
				}
			}

			if shouldParse {
				return e.ingestFile(p)
			}
			return e.ingestRawFile(p)
		})
	}
	return e.ingestFile(path)
}

func (e *Engine) ingestFile(path string) error {
	ext := filepath.Ext(path)

	switch ext {
	case ".db":
		return e.ingestSQLiteStreaming(path)
	case ".json":
		return e.ingestJSON(path)
	case ".py":
		return e.ingestTreeSitter(path, python.GetLanguage(), "python")
	case ".js":
		return e.ingestTreeSitter(path, javascript.GetLanguage(), "javascript")
	case ".ts", ".tsx":
		// Use Typescript grammar for both .ts and .tsx (it handles JSX mostly, or use tsx grammar if strictly needed)
		// go-tree-sitter/typescript usually has typescript and tsx subpackages.
		// For now, use typescript.
		return e.ingestTreeSitter(path, typescript.GetLanguage(), "typescript")
	case ".sql":
		return e.ingestTreeSitter(path, sql.GetLanguage(), "sql")
	case ".go":
		return e.ingestTreeSitter(path, golang.GetLanguage(), "go")
	case ".tf", ".hcl":
		return e.ingestTreeSitter(path, hcl.GetLanguage(), "hcl")
	case ".yaml", ".yml":
		return e.ingestTreeSitter(path, yaml.GetLanguage(), "yaml")
	case ".rs":
		return e.ingestTreeSitter(path, rust.GetLanguage(), "rust")
	default:
		return e.ingestRawFile(path)
	}
}

func (e *Engine) ingestJSON(path string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// Use time.Now() to force NFS cache invalidation
	modTime := time.Now()

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
		if err := e.processNode(nodeSchema, walker, data, "", "", modTime, e.Store); err != nil {
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

func (e *Engine) ingestTreeSitter(path string, lang *sitter.Language, langName string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	realPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		realPath = absPath
	}

	content, err := os.ReadFile(realPath)
	if err != nil {
		return err
	}
	// Use time.Now() to force NFS cache invalidation on updates
	modTime := time.Now()

	// Use buffering target for atomic swap
	// Note: We do NOT call DeleteFileNodes here anymore.
	// ReplaceFileNodes will handle deletion + addition atomically.
	bt := &bufferingTarget{IngestionTarget: e.Store}

	parser := sitter.NewParser()
	parser.SetLanguage(lang)
	tree, err := parser.ParseCtx(context.Background(), nil, content)
	if err != nil {
		log.Printf("ingest: parse failed for %s (using raw fallback): %v", path, err)
	}

	if err == nil {
		walker := NewSitterWalker()
		root := SitterRoot{Node: tree.RootNode(), FileRoot: tree.RootNode(), Source: content, Lang: lang, LangName: langName}
		sourceFile := filepath.Base(path)
		e.sourceFile = realPath
		defer func() { e.sourceFile = "" }()

		// Use bt (buffering target) instead of e.Store inside loop
		// We need to temporarily swap e.Store or pass bt?
		// processNode uses e.Store directly.
		// I should change processNode to take a store argument?
		// Or simpler: e.Store is an interface. I can swap it on the Engine struct temporarily.
		// But Engine is shared? No, Engine is created per mount?
		// Engine is created in cmd/mount.go.
		// If concurrent ingests happen on same Engine?
		// Ingest is called by Release (write-back).
		// If multiple write-backs happen, they race on e.Store swap.
		// Engine struct shouldn't be mutated if shared.
		// BUT processNode reads e.Store.

		// Solution: Pass store to processNode.
		// I will update processNode signature.

		for _, nodeSchema := range e.Schema.Nodes {
			if err := e.processNode(nodeSchema, walker, root, "", sourceFile, modTime, bt); err != nil {
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

func (e *Engine) ingestRawFile(path string) error {
	rel, err := filepath.Rel(e.RootPath, path)
	if err != nil {
		return err
	}
	rel = filepath.ToSlash(rel)
	parts := strings.Split(rel, "/")

	parentID := "" // Root starts empty

	// 1. Create/Ensure intermediate directories
	for i := 0; i < len(parts)-1; i++ {
		part := parts[i]
		currentID := part
		if parentID != "" {
			currentID = parentID + "/" + part
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
					// Check if child already linked (dedup)
					exists := false
					for _, c := range parent.Children {
						if c == currentID {
							exists = true
							break
						}
					}
					if !exists {
						parent.Children = append(parent.Children, currentID)
						e.Store.AddNode(parent)
					}
				}
			}
		}
		parentID = currentID
	}

	// 2. Create file node
	fileID := rel
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	// Use time.Now() to force NFS cache invalidation
	modTime := time.Now()

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

	// Workers: parse JSON, render templates, build nodes
	var workerWg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		workerWg.Add(1)
		go func() {
			defer workerWg.Done()
			w := NewJsonWalker()
			for job := range jobs {
				results <- processRecord(e.Schema, w, dbPath, job)
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
				fmt.Printf("\rProcessed %d records...", count)
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
		}
		fmt.Printf("\rProcessed %d records... Done.\n", count)
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
func processRecord(schema *api.Topology, walker Walker, dbPath string, job recordJob) recordResult {
	var parsed any
	if err := json.Unmarshal([]byte(job.raw), &parsed); err != nil {
		return recordResult{err: fmt.Errorf("parse record %s: %w", job.recordID, err)}
	}

	wrapper := []any{parsed}
	var result recordResult

	for _, nodeSchema := range schema.Nodes {
		for _, childSchema := range nodeSchema.Children {
			collectNodes(&result, childSchema, walker, wrapper, nodeSchema.Name, dbPath, job.recordID)
			if result.err != nil {
				return result
			}
		}
	}

	return result
}

// collectNodes is the pure equivalent of processNode — builds node lists
// without any store access. Safe to call from multiple goroutines.
func collectNodes(result *recordResult, schema api.Node, walker Walker, ctx any, parentPath, dbPath, recordID string) {
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
		id := strings.TrimPrefix(filepath.ToSlash(currentPath), "/")

		node := &graph.Node{
			ID:   id,
			Mode: os.ModeDir | 0o555,
		}

		// Recurse children
		nextCtx := match.Context()
		if nextCtx != nil {
			for _, childSchema := range schema.Children {
				collectNodes(result, childSchema, walker, nextCtx, currentPath, dbPath, recordID)
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
			fileId := strings.TrimPrefix(filepath.ToSlash(filePath), "/")

			content, err := RenderTemplate(fileSchema.ContentTemplate, match.Values())
			if err != nil {
				log.Printf("collectNodes: skip file content render %q: %v", fileId, err)
				continue
			}

			fileNode := &graph.Node{
				ID:   fileId,
				Mode: 0o444,
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

		// Link to parent (collector will apply this)
		parentID := strings.TrimPrefix(filepath.ToSlash(parentPath), "/")
		result.parentLinks = append(result.parentLinks, parentLink{childID: id, parentID: parentID})
	}
}

// dedupSuffix returns a ".from_<sanitized>" suffix derived from the source filename.
// Dots in the filename are replaced with underscores to avoid path separator confusion.
// e.g., "a.go" -> ".from_a_go"
func dedupSuffix(sourceFile string) string {
	sanitized := strings.ReplaceAll(sourceFile, ".", "_")
	return ".from_" + sanitized
}

func (e *Engine) processNode(schema api.Node, walker Walker, ctx any, parentPath, sourceFile string, modTime time.Time, store IngestionTarget) error {
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
		id := strings.TrimPrefix(filepath.ToSlash(currentPath), "/")

		// Dedup: when this node has files and a node with the same ID
		// already exists with file children (i.e., from a different source file),
		// append a source-file suffix to disambiguate.
		// This handles cases like multiple init() functions across Go files.
		if len(schema.Files) > 0 && sourceFile != "" {
			if existing, err := store.GetNode(id); err == nil && len(existing.Children) > 0 {
				suffix := dedupSuffix(sourceFile)
				name = name + suffix
				currentPath = filepath.Join(parentPath, name)
				id = strings.TrimPrefix(filepath.ToSlash(currentPath), "/")
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
		store.AddNode(node)

		// Link to parent
		if parentPath == "" {
			store.AddRoot(node)
		} else {
			parentId := strings.TrimPrefix(filepath.ToSlash(parentPath), "/")
			parent, err := store.GetNode(parentId)
			if err == nil {
				exists := false
				for _, c := range parent.Children {
					if c == id {
						exists = true
						break
					}
				}
				if !exists {
					parent.Children = append(parent.Children, id)
					store.AddNode(parent)
				}
			}
		}

		// Recurse children
		nextCtx := match.Context()
		if nextCtx != nil {
			for _, childSchema := range schema.Children {
				if err := e.processNode(childSchema, walker, nextCtx, currentPath, sourceFile, modTime, store); err != nil {
					return err
				}
			}
		}

		// Optimization: extract calls once per match if we are in Sitter mode
		var calls []string
		var contextData []byte
		if sw, ok := walker.(*SitterWalker); ok {
			ctxAny := match.Context()
			if ctxAny == nil {
				log.Printf("Context is nil for %s", id)
			} else if root, ok := ctxAny.(SitterRoot); ok {
				c, err := sw.ExtractCalls(root.Node, root.Source, root.Lang, root.LangName)
				if err == nil {
					calls = c
				}
				// Extract context (imports, globals) from the file root
				// Use the file root node, not the match scope
				fileRoot := root.FileRoot
				if fileRoot == nil {
					fileRoot = root.Node
				}
				ctxBytes, err := sw.ExtractContext(fileRoot, root.Source, root.Lang, root.LangName)
				if err == nil {
					contextData = ctxBytes
					if len(ctxBytes) > 0 {
						log.Printf("Context extracted for %s: %d bytes", id, len(ctxBytes))
					} else {
						log.Printf("Context extracted for %s: 0 bytes", id)
					}
				} else {
					log.Printf("Context extraction failed for %s: %v", id, err)
				}
			} else {
				log.Printf("Context type mismatch for %s: %T", id, ctxAny)
			}
		} else {
			log.Printf("Walker is not SitterWalker for %s", id)
		}

		// Re-fetch current children (updated by recursion)
		var currentChildren []string
		if current, err := store.GetNode(id); err == nil {
			currentChildren = current.Children
		}

		node = &graph.Node{
			ID:       id,
			Mode:     os.ModeDir | 0o555, // Read-only dir
			ModTime:  modTime,            // Propagate source file time
			Children: currentChildren,
			Context:  contextData,
		}
		store.AddNode(node)
		for _, fileSchema := range schema.Files {
			fileName, err := RenderTemplate(fileSchema.Name, match.Values())
			if err != nil {
				log.Printf("processNode: skip file name render %q: %v", fileSchema.Name, err)
				continue
			}
			filePath := filepath.Join(currentPath, fileName)
			fileId := strings.TrimPrefix(filepath.ToSlash(filePath), "/")

			content, err := RenderTemplate(fileSchema.ContentTemplate, match.Values())
			if err != nil {
				log.Printf("processNode: skip file content render %q: %v", fileId, err)
				continue
			}

			fileNode := &graph.Node{
				ID:      fileId,
				Mode:    0o444,
				ModTime: modTime,
				Data:    []byte(content),
			}

			// Populate Origin from tree-sitter captures for write-back
			if op, ok := match.(OriginProvider); ok && e.sourceFile != "" {
				// Extend Backward to capture preceding comments (Doc Comments)
				// Access raw node via sitterMatch if possible
				if sm, ok := match.(interface{ GetCaptureNode(string) *sitter.Node }); ok {
					if node := sm.GetCaptureNode("scope"); node != nil {
						startByte := node.StartByte()

						// Walk backward to find contiguous comments
						prev := node.PrevSibling()
						for prev != nil && prev.Type() == "comment" {
							// Check adjacency: <= 2 bytes gap (allow \n or \n\n)
							// Note: Tree-sitter byte ranges are accurate.
							if int(node.StartByte())-int(prev.EndByte()) <= 2 {
								startByte = prev.StartByte()
								// Update node to prev to keep walking back
								node = prev // Just for loop logic, we use startByte
								prev = prev.PrevSibling()
							} else {
								break
							}
						}

						// If we extended the range, we must also update the CONTENT (Data)
						// because the current 'content' (from RenderTemplate) only includes the original scope.
						// We need to re-read from the source file in memory.
						if startByte < sm.GetCaptureNode("scope").StartByte() {
							// We need the source content.
							// Fortunately, match.Context() is SitterRoot which has Source!
							if root, ok := match.Context().(SitterRoot); ok {
								// Re-slice content
								endByte := sm.GetCaptureNode("scope").EndByte()
								if endByte <= uint32(len(root.Source)) {
									extendedContent := root.Source[startByte:endByte]
									fileNode.Data = []byte(extendedContent)
								}
							}
						}

						fileNode.Origin = &graph.SourceOrigin{
							FilePath:  e.sourceFile,
							StartByte: startByte,
							EndByte:   sm.GetCaptureNode("scope").EndByte(),
						}
					}
				} else if start, end, ok := op.CaptureOrigin("scope"); ok {
					// Fallback for non-sitter matches (shouldn't happen here but safe)
					fileNode.Origin = &graph.SourceOrigin{
						FilePath:  e.sourceFile,
						StartByte: start,
						EndByte:   end,
					}
				}
			}

			store.AddNode(fileNode)
			node.Children = append(node.Children, fileId)
			store.AddNode(node)

			// Update Index
			for _, token := range calls {
				if err := store.AddRef(token, fileId); err != nil {
					return fmt.Errorf("add ref %s -> %s: %w", token, fileId, err)
				}
			}
		}
	}
	return nil
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
