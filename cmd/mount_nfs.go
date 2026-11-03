package cmd

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/leyline"
	"github.com/agentic-research/mache/internal/linter"
	"github.com/agentic-research/mache/internal/nfsmount"
	"github.com/agentic-research/mache/internal/writeback"
)

// mountNFS starts an NFS server backed by GraphFS and mounts it.
func mountNFS(schema *api.Topology, g graph.Graph, engine *ingest.Engine, mountPoint string, writable bool, promptContent []byte) error {
	graphFs := nfsmount.NewGraphFS(g, schema)
	if len(promptContent) > 0 {
		graphFs.SetPromptContent(promptContent)
	}

	// Wire write-back if requested (validate → format → splice → surgical update → invalidate)
	if writable && engine != nil {
		graphFs.SetWriteBack(spliceWriteBack(g))
		log.Println("Write-back enabled: edits will splice into source files.")
	} else if writable {
		log.Println("Warning: --writable ignored (only supported for non-.db sources)")
	}

	srv, err := nfsmount.NewServer(graphFs)
	if err != nil {
		return fmt.Errorf("start NFS server: %w", err)
	}
	defer func() { _ = srv.Close() }()

	log.Printf("Mounting mache at %s (NFS on localhost:%d)...", mountPoint, srv.Port())

	if err := nfsmount.Mount(srv.Port(), mountPoint, writable, nfsOpts); err != nil {
		return err
	}
	log.Print("Mounted. Press Ctrl-C to unmount.")

	// Block until signal
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Printf("Unmounting %s...", mountPoint)
	if err := nfsmount.Unmount(mountPoint); err != nil {
		log.Printf("Warning: unmount failed: %v", err)
		log.Printf("Run manually: sudo umount %s", mountPoint)
	}
	return nil
}

// FUSE backend removed in v0.7.0 (ADR-0006). NFS is the only mount backend.
// For FUSE mounts, use ley-line-open's `leyline serve`.

// spliceWriteBack is the write-back for a mount over source files: validate,
// format, splice into the source file, then update the node in place. The
// draft, lint and status bookkeeping lives on a MemoryStore; on any other
// graph it is skipped.
func spliceWriteBack(g graph.Graph) nfsmount.WriteBackFunc {
	store, _ := g.(*graph.MemoryStore)
	return func(nodeID string, origin graph.SourceOrigin, content []byte) error {
		node, err := g.GetNode(nodeID)
		if err != nil {
			return fmt.Errorf("node not found: %w", err)
		}
		// 1. Validate before touching the source file. One leyline parse
		// serves both validation and, for Go, the lint rules below.
		astPayload, drafted, err := validateOrDraft(store, node, origin, content)
		if err != nil || drafted {
			return err
		}
		// 2. Format in-process (gofumpt for Go, hclwrite for HCL/Terraform).
		formatted := writeback.FormatBuffer(content, origin.FilePath)
		recordLint(store, nodeID, origin.FilePath, astPayload)
		// 3. Splice the formatted content into the source file.
		oldLen := origin.EndByte - origin.StartByte
		if err := writeback.Splice(origin, formatted); err != nil {
			return err
		}
		// 4. Surgical node update, no re-ingest; 5. invalidate caches.
		applySplice(store, nodeID, origin, formatted, oldLen)
		g.Invalidate(nodeID)
		return nil
	}
}

// validateOrDraft validates content. A syntax error keeps the edit as the
// node's draft, with _diagnostics saying why (#527), and reports drafted. The
// validator itself not answering (daemon unreachable or too old) fails closed:
// an error for the writer and no draft, so the mount keeps serving the source
// as it is on disk. Drafting there reported success while builds still saw
// the old code (mache-3a213e).
func validateOrDraft(store *graph.MemoryStore, node *graph.Node, origin graph.SourceOrigin, content []byte) (*leyline.ASTPayload, bool, error) {
	dir := filepath.Dir(node.ID)
	astPayload, err := writeback.ValidateWithAST(content, origin.FilePath)
	if err == nil {
		return astPayload, false, nil
	}
	var ve *writeback.ValidationError
	if !errors.As(err, &ve) {
		log.Printf("writeback: validator unavailable for %s: %v (write refused)", origin.FilePath, err)
		if store != nil {
			store.WriteStatus.Store(dir, "validator unavailable (write refused, source untouched): "+err.Error())
		}
		return nil, false, fmt.Errorf("validator unavailable, write refused: %w", err)
	}
	log.Printf("writeback: validation failed for %s: %v (saving draft)", origin.FilePath, err)
	if store != nil {
		store.WriteStatus.Store(dir, err.Error())
		node.DraftData = slices.Clone(content)
	}
	return nil, true, nil
}

// recordLint stores the Go lint findings for a write as a warning in
// _diagnostics (or clears the last ones). It reads the pre-format AST: for the
// snippet writes this path receives, FormatBuffer is a no-op (format.Source
// needs a whole file), so line numbers agree.
func recordLint(store *graph.MemoryStore, nodeID, filePath string, astPayload *leyline.ASTPayload) {
	if store == nil || !strings.HasSuffix(filePath, ".go") {
		return
	}
	key := filepath.Dir(nodeID) + "/lint"
	diags, err := linter.LintAST(astPayload)
	if err != nil || len(diags) == 0 {
		store.WriteStatus.Delete(key)
		return
	}
	var sb strings.Builder
	for _, d := range diags {
		sb.WriteString(d.String() + "\n")
	}
	store.WriteStatus.Store(key, sb.String())
}

// applySplice updates the node in place after a splice: shifts the origins
// after it, stores the new content under the source file's mtime, and records
// the outcome in _diagnostics. The splice has already reached disk, so a
// failure here leaves the graph stale and says so.
func applySplice(store *graph.MemoryStore, nodeID string, origin graph.SourceOrigin, formatted []byte, oldLen uint32) {
	if store == nil {
		return
	}
	newOrigin := &graph.SourceOrigin{
		FilePath:  origin.FilePath,
		StartByte: origin.StartByte,
		EndByte:   origin.StartByte + uint32(len(formatted)),
	}
	if delta := int32(len(formatted)) - int32(oldLen); delta != 0 {
		store.ShiftOrigins(origin.FilePath, origin.EndByte, delta)
	}
	modTime := time.Now()
	if fi, err := os.Stat(origin.FilePath); err == nil {
		modTime = fi.ModTime()
	}
	if err := store.UpdateNodeContent(nodeID, formatted, newOrigin, modTime); err != nil {
		log.Printf("writeback: graph update failed after splice for %s: %v", nodeID, err)
		store.WriteStatus.Store(filepath.Dir(nodeID),
			fmt.Sprintf("graph stale (splice succeeded, graph update failed): %v", err))
	} else {
		store.WriteStatus.Store(filepath.Dir(nodeID), "ok")
	}
	store.RecordFileMtime(origin.FilePath, modTime)
}
