// Package leyline — trigger.go pushes graph content to the ley-line daemon
// for embedding. Called as a fire-and-forget goroutine after graph construction.
package leyline

import (
	"log"

	"github.com/agentic-research/mache/internal/graph"
)

// TriggerEmbedding walks all file nodes in the graph and pushes their content
// to the ley-line daemon for embedding via the embed_content op.
// Batches nodes in groups of batchSize. Logs errors but does not fail.
func TriggerEmbedding(g graph.Graph, batchSize int) {
	if batchSize <= 0 {
		batchSize = 100
	}

	sockPath, err := DiscoverOrStart()
	if err != nil {
		log.Printf("embed trigger: ley-line not available: %v", err)
		return
	}

	sock, err := DialSocket(sockPath)
	if err != nil {
		log.Printf("embed trigger: connect failed: %v", err)
		return
	}
	defer func() { _ = sock.Close() }()

	sc := NewSemanticClient(sock)

	// Check if embeddings are enabled
	status, err := sc.Status()
	if err != nil || !status.Ready {
		log.Printf("embed trigger: embeddings not enabled on ley-line daemon")
		return
	}

	// Walk all file nodes and collect content
	var batch []NodeContent
	var total int

	var walkDir func(id string)
	walkDir = func(id string) {
		children, err := g.ListChildren(id)
		if err != nil {
			return
		}
		for _, childID := range children {
			node, err := g.GetNode(childID)
			if err != nil || node == nil {
				continue
			}
			if node.Mode.IsDir() {
				walkDir(childID)
				continue
			}

			// Read content
			buf := make([]byte, 8192)
			n, _ := g.ReadContent(childID, buf, 0)
			if n == 0 {
				continue
			}
			content := string(buf[:n])

			batch = append(batch, NodeContent{ID: childID, Content: content})

			if len(batch) >= batchSize {
				embedded, err := sc.EmbedContent(batch)
				if err != nil {
					log.Printf("embed trigger: batch error: %v", err)
				} else {
					total += embedded
				}
				batch = batch[:0]
			}
		}
	}

	// Start from roots
	roots, err := g.ListChildren("")
	if err != nil {
		log.Printf("embed trigger: list roots: %v", err)
		return
	}
	for _, root := range roots {
		node, err := g.GetNode(root)
		if err != nil || node == nil {
			continue
		}
		if node.Mode.IsDir() {
			walkDir(root)
		}
	}

	// Flush remaining batch
	if len(batch) > 0 {
		embedded, err := sc.EmbedContent(batch)
		if err != nil {
			log.Printf("embed trigger: final batch error: %v", err)
		} else {
			total += embedded
		}
	}

	if total > 0 {
		log.Printf("embed trigger: pushed %d nodes to ley-line for embedding", total)
	}
}
