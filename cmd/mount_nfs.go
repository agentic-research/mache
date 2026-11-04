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
		graphFs.SetWriteBack(proposeWriteBack(g))
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

// proposeWriteBack is the write-back for a mount over source files: validate,
// format, then propose the node's file with the edit through leyline, against
// the blob the node was read from (writeback.Propose), and update the node in
// place. The daemon writes the file; mache does not (mache-3a0fb9). The draft,
// lint and status bookkeeping lives on a MemoryStore; on any other graph there
// is no base to propose against, and the write is refused.
func proposeWriteBack(g graph.Graph) nfsmount.WriteBackFunc {
	store, _ := g.(*graph.MemoryStore)
	return func(nodeID string, origin graph.SourceOrigin, content []byte) error {
		node, err := g.GetNode(nodeID)
		if err != nil {
			return fmt.Errorf("node not found: %w", err)
		}
		// 1. Validate before proposing. One leyline parse serves both
		// validation and, for Go, the lint rules below.
		astPayload, drafted, err := validateOrDraft(store, node, origin, content)
		if err != nil || drafted {
			return err
		}
		if store == nil {
			return writeback.ErrNoBase
		}
		// 2. Format in-process (gofumpt for Go, hclwrite for HCL/Terraform).
		formatted := writeback.FormatBuffer(content, origin.FilePath)
		recordLint(store, nodeID, origin.FilePath, astPayload)
		// 3. Propose; 4. record the accepted write; 5. invalidate caches.
		a, err := writeback.Propose(origin, formatted, store.Blobs())
		if err != nil {
			recordRefusal(store, nodeID, err)
			return err
		}
		applyProposal(store, nodeID, origin, a)
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

// applyProposal records an accepted write (writeback.Commit) under the
// source file's mtime, which the daemon set when it wrote the file, and says
// how it went in _diagnostics. A merged write is not what mache built: the
// node keeps the edit, and the file must be re-read before it is written
// again.
func applyProposal(store *graph.MemoryStore, nodeID string, origin graph.SourceOrigin, a *writeback.Applied) {
	modTime := time.Now()
	if fi, err := os.Stat(origin.FilePath); err == nil {
		modTime = fi.ModTime()
	}
	dir := filepath.Dir(nodeID)
	switch err := writeback.Commit(store, nodeID, origin, a, modTime); {
	case err != nil:
		log.Printf("writeback: graph update failed after propose for %s: %v", nodeID, err)
		store.WriteStatus.Store(dir, fmt.Sprintf("graph stale (write applied, graph update failed): %v", err))
	case a.Merged:
		store.WriteStatus.Store(dir, "ok, merged with a change made since this was read: re-read the file before writing it again")
	default:
		store.WriteStatus.Store(dir, "ok")
	}
	store.RecordFileMtime(origin.FilePath, modTime)
}

// recordRefusal says in _diagnostics why a write was refused; a stale one
// names the lines it overlapped another writer's change on (1-based).
func recordRefusal(store *graph.MemoryStore, nodeID string, err error) {
	msg := "write refused, source untouched: " + err.Error()
	var r *writeback.RefusedError
	if errors.As(err, &r) {
		for _, m := range r.Refusal.Moved {
			for _, c := range m.Conflicts {
				msg += fmt.Sprintf("\n%s: lines %d-%d overlap a change made since it was read", m.Path, c.Start+1, c.End)
			}
		}
	}
	store.WriteStatus.Store(filepath.Dir(nodeID), msg)
}
