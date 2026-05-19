package cmd

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/control"
	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/nfsmount"
	machetmpl "github.com/agentic-research/mache/internal/template"
)

// mountControl starts Mache in hot-swap mode using the Control Block.
func mountControl(path string, schema *api.Topology, mountPoint string) error {
	ctrl, err := control.OpenOrCreate(path)
	if err != nil {
		return fmt.Errorf("open control: %w", err)
	}
	defer func() { _ = ctrl.Close() }()

	// Initial Load: read the controller's current arena pointer + root.
	// The root is the substrate identity; a zero root means no snapshot
	// has been published yet (fresh controller).
	initialRoot := ctrl.GetCurrentRoot()
	arenaPath := ctrl.GetArenaPath()
	log.Printf("Control Block: root %s -> %s", shortRoot(initialRoot), arenaPath)

	// Wait for first valid arena if empty
	if arenaPath == "" {
		log.Println("Waiting for initial arena...")
		deadline := time.After(30 * time.Second)
		for {
			select {
			case <-deadline:
				return fmt.Errorf("timed out waiting for initial arena (30s)")
			default:
			}
			if p := ctrl.GetArenaPath(); p != "" {
				arenaPath = p
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	// Writable mode still extracts the active buffer to a temp file —
	// WritableGraph splices edits into a real SQLite connection before
	// flushing back to the arena, and the daemon doesn't expose write
	// ops over UDS. Read-only mode skips this entirely and queries the
	// daemon via UDS.
	if writable {
		log.Println("Waiting for valid arena header (writable mode)...")
		var dbPath string
		deadline := time.After(30 * time.Second)
		for {
			var extractErr error
			dbPath, extractErr = graph.ExtractActiveDB(arenaPath)
			if extractErr == nil {
				break
			}
			select {
			case <-deadline:
				return fmt.Errorf("timed out waiting for valid arena header (30s): %w", extractErr)
			default:
			}
			time.Sleep(500 * time.Millisecond)
		}
		log.Println("Arena header valid. Initializing writable graph.")
		return mountControlWritable(dbPath, arenaPath, schema, ctrl, mountPoint)
	}

	// Read-only mode: query the daemon over UDS. The daemon owns SQLite
	// (zero-copy via sqlite3_deserialize on the arena); mache never opens
	// a local SQLite file. Hot-swap is implicit — each query reflects
	// the daemon's current snapshot, so root bumps don't need a graph
	// swap on the mache side. LLO 0.2.2 added `find_callees` so callees-
	// dependent features (`/callees` vfs, `find_impact`, `find_smells`
	// call-graph rules) work over UDS too. Closes mache-98b9bf.
	sockPath := strings.TrimSuffix(path, ".ctrl") + ".sock"
	log.Printf("Waiting for daemon socket %s...", sockPath)
	// Loop on the dial itself rather than stat'ing the socket file:
	// (a) a stale leftover .sock from a previous daemon will pass stat
	//     but fail to dial — retry lets us wait through the new daemon
	//     re-creating it;
	// (b) the file can exist before the daemon has actually started
	//     accepting connections (bind-then-listen race), so the first
	//     dial often hits ECONNREFUSED even when subsequent ones work.
	// 30s deadline matches the prior stat-based loop.
	var g *udsGraph
	sockDeadline := time.Now().Add(30 * time.Second)
	for {
		var dialErr error
		g, dialErr = newUDSGraph(sockPath)
		if dialErr == nil {
			break
		}
		if time.Now().After(sockDeadline) {
			return fmt.Errorf("timed out connecting to daemon socket %s after 30s: %w", sockPath, dialErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
	defer func() { _ = g.Close() }()
	log.Printf("Connected to ley-line daemon at %s", sockPath)

	return mountNFS(schema, g, nil, mountPoint, false, nil)
}

// shortRoot renders the first 4 bytes of a 32-byte BLAKE3 root as 8 hex
// chars, or "<zero>" for the all-zero sentinel — readable enough for log
// lines without dragging full 64-char hex everywhere.
func shortRoot(root [32]byte) string {
	if control.IsZeroRoot(root) {
		return "<zero>"
	}
	return fmt.Sprintf("%02x%02x%02x%02x", root[0], root[1], root[2], root[3])
}

// mountControlWritable opens the extracted DB in read-write mode and
// wires a WritableGraph + ArenaFlusher for arena write-back.
func mountControlWritable(masterDBPath, arenaPath string, schema *api.Topology, ctrl *control.Controller, mountPoint string) error {
	flusher := graph.NewArenaFlusher(arenaPath, masterDBPath, ctrl)
	flusher.Start(100 * time.Millisecond)
	defer func() { _ = flusher.Close() }() // final flush on unmount

	wg, err := graph.OpenWritableGraph(masterDBPath, schema, machetmpl.Render, flusher)
	if err != nil {
		return fmt.Errorf("open writable graph: %w", err)
	}
	defer func() { _ = wg.Close() }()

	log.Println("Writable arena mode: edits write to master DB and flush to arena (100ms coalesce).")

	return mountWritableNFS(schema, wg, mountPoint)
}

// mountWritableNFS mounts a WritableGraph via NFS with arena write-back.
func mountWritableNFS(schema *api.Topology, wg *graph.WritableGraph, mountPoint string) error {
	graphFs := nfsmount.NewGraphFS(wg, schema)

	graphFs.SetWriteBack(func(nodeID string, origin graph.SourceOrigin, content []byte) error {
		// Update DB record, then request coalesced arena flush (non-blocking).
		if err := wg.UpdateRecord(nodeID, content); err != nil {
			return fmt.Errorf("update record: %w", err)
		}
		wg.Flush() // coalesced — actual I/O on next tick
		return nil
	})

	srv, err := nfsmount.NewServer(graphFs)
	if err != nil {
		return fmt.Errorf("start NFS server: %w", err)
	}
	defer func() { _ = srv.Close() }()

	log.Printf("Mounting mache at %s (NFS on localhost:%d)...", mountPoint, srv.Port())

	if err := nfsmount.Mount(srv.Port(), mountPoint, true, nfsOpts); err != nil {
		return err
	}
	log.Print("Mounted (writable). Press Ctrl-C to unmount.")

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
