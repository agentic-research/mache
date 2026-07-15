package cmd

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/ingest"
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
		store, isMemStore := g.(*graph.MemoryStore)
		graphFs.SetWriteBack(func(nodeID string, origin graph.SourceOrigin, content []byte) error {
			// Retrieve node to update DraftData
			node, err := g.GetNode(nodeID)
			if err != nil {
				return fmt.Errorf("node not found: %w", err)
			}

			// 1. Validate syntax before touching source file. ONE leyline
			// parse serves both validation and (for Go) the lint rules
			// below via the returned AST payload — no second parse.
			astPayload, err := writeback.ValidateWithAST(content, origin.FilePath)
			if err != nil {
				// Syntax failure and validator-infra failure (daemon
				// unreachable / too old) both draft — the pre-existing
				// contract — but the _diagnostics message distinguishes
				// them so an editor user can tell "my code is broken"
				// from "the validator is down" (#527 review).
				diag := err.Error()
				var ve *writeback.ValidationError
				if !errors.As(err, &ve) {
					diag = "validator unavailable (draft saved, source untouched): " + diag
				}
				log.Printf("writeback: validation failed for %s: %v (saving draft)", origin.FilePath, err)
				// Store diagnostic for _diagnostics/ virtual dir
				if isMemStore {
					store.WriteStatus.Store(filepath.Dir(nodeID), diag)
					// Save as Draft
					draft := make([]byte, len(content))
					copy(draft, content)
					node.DraftData = draft
				}
				return nil
			}

			// 2. Format in-process (gofumpt for Go, hclwrite for HCL/Terraform)
			formatted := writeback.FormatBuffer(content, origin.FilePath)

			// Linting (Warning only). Runs on the pre-format AST — for the
			// snippet writes this path receives, FormatBuffer is a no-op
			// (format.Source needs a whole file), so line numbers agree;
			// any residual drift is acceptable for a warning-only signal.
			if strings.HasSuffix(origin.FilePath, ".go") {
				if diags, err := linter.LintAST(astPayload); err == nil && len(diags) > 0 {
					var sb strings.Builder
					for _, d := range diags {
						sb.WriteString(d.String() + "\n")
					}
					store.WriteStatus.Store(filepath.Dir(nodeID)+"/lint", sb.String())
				} else {
					store.WriteStatus.Delete(filepath.Dir(nodeID) + "/lint")
				}
			}

			// 3. Splice formatted content into source file
			oldLen := origin.EndByte - origin.StartByte
			if err := writeback.Splice(origin, formatted); err != nil {
				return err
			}

			// 4. Surgical node update — no re-ingest
			newOrigin := &graph.SourceOrigin{
				FilePath:  origin.FilePath,
				StartByte: origin.StartByte,
				EndByte:   origin.StartByte + uint32(len(formatted)),
			}
			if isMemStore {
				delta := int32(len(formatted)) - int32(oldLen)
				if delta != 0 {
					store.ShiftOrigins(origin.FilePath, origin.EndByte, delta)
				}
				// Use source file mtime for deterministic timestamps
				modTime := time.Now()
				if fi, err := os.Stat(origin.FilePath); err == nil {
					modTime = fi.ModTime()
				}
				if err := store.UpdateNodeContent(nodeID, formatted, newOrigin, modTime); err != nil {
					// Splice already touched disk; the graph is now stale.
					// Surface via WriteStatus so the _diagnostics virtual
					// dir reflects reality (file on disk is correct,
					// in-memory graph is wrong, re-read to refresh).
					log.Printf("writeback: graph update failed after splice for %s: %v", nodeID, err)
					store.WriteStatus.Store(filepath.Dir(nodeID),
						fmt.Sprintf("graph stale (splice succeeded, graph update failed): %v", err))
				} else {
					store.WriteStatus.Store(filepath.Dir(nodeID), "ok")
				}
				store.RecordFileMtime(origin.FilePath, modTime)
			}

			// 5. Invalidate cached size/content
			g.Invalidate(nodeID)
			return nil
		})
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
