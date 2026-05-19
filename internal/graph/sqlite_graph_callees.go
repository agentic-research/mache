package graph

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// GetCallees implements Graph. It extracts call sites from the construct's
// source file and resolves each token to one or more definition node IDs
// using a fallback chain:
//
//  1. Qualified lookup (e.g. "auth.Validate") via in-memory defs.
//  2. SQL fallback to node_defs (pre-built .db only).
//  3. Bare-token lookup via in-memory defs.
//  4. Bare-token SQL lookup against node_defs.
//  5. Suffix-match in node_defs ("*.token") for unambiguous receiver-method
//     calls — see bead mache-9ca6af for the receiver-method root cause.
//  6. Final fallback: name match in the nodes table.
func (g *SQLiteGraph) GetCallees(id string) ([]*Node, error) {
	id = NormalizeID(id)

	// 1. Find the "source" file child
	children, err := g.ListChildren(id)
	if err != nil {
		return nil, nil
	}

	var sourceID string
	for _, child := range children {
		base := filepath.Base(child)
		if base == "source" {
			// Both backends now return full paths from ListChildren —
			// SQLiteGraph forwards to NodesTableReader which selects the
			// `id` column (full path); the sidecar path also stores full
			// paths in dirChildren. Earlier this branch concatenated
			// `id + "/" + child` for useNodesTable assuming bare names —
			// produced "leyline/methods/SendOp/leyline/methods/SendOp/source"
			// and made find_callees silently return [] for every construct
			// on a pre-built .db. Bead mache-9ca6af root cause.
			sourceID = child
			break
		}
	}
	if sourceID == "" {
		return nil, nil
	}

	// 2. Read content
	segments := strings.Split(sourceID, "/")
	_, fileLeaf := g.walkSchema(segments)

	if fileLeaf == nil && !g.useNodesTable {
		return nil, nil
	}

	content, err := g.resolveContent(sourceID, segments, fileLeaf)
	if err != nil {
		return nil, nil
	}

	// 3. Determine langName from construct node Properties (stored in record column)
	var langName string
	if g.useNodesTable {
		var recordJSON sql.NullString
		// kind = 1 → graph.NodeKindDir (constructs are dirs).
		_ = g.db.QueryRow("SELECT record FROM nodes WHERE id = ? AND kind = 1", id).Scan(&recordJSON)
		if recordJSON.Valid && recordJSON.String != "" {
			var props map[string][]byte
			if json.Unmarshal([]byte(recordJSON.String), &props) == nil {
				if lang, ok := props["lang"]; ok {
					langName = string(lang)
				}
			}
		}
	}

	// 4. Extract qualified calls
	if g.extractor == nil {
		return nil, nil
	}
	qcalls, err := g.extractor(content, sourceID, langName)
	if err != nil {
		return nil, fmt.Errorf("extract calls: %w", err)
	}
	if len(qcalls) == 0 {
		return nil, nil
	}

	// 5. Resolve tokens to definition nodes (qualified → bare → SQL fallback)
	var nodes []*Node
	seen := make(map[string]bool)

	// Snapshot defs under lock — deep copy slices to prevent races with concurrent AddDef.
	var defs map[string][]string
	g.pendingMu.Lock()
	if g.defs != nil {
		defs = make(map[string][]string, len(g.defs))
		for k, v := range g.defs {
			cp := make([]string, len(v))
			copy(cp, v)
			defs[k] = cp
		}
	}
	g.pendingMu.Unlock()

	for _, qc := range qcalls {
		resolved := false

		// Qualified resolution: "auth.Validate" → defs["auth.Validate"]
		if qc.Qualifier != "" {
			qualKey := qc.Qualifier + "." + qc.Token
			if defs != nil {
				if defIDs, ok := defs[qualKey]; ok {
					for _, defID := range defIDs {
						if defID == id || seen[defID] {
							continue
						}
						seen[defID] = true
						nodes = append(nodes, &Node{ID: defID, Mode: os.ModeDir | 0o555})
						resolved = true
					}
				}
			}
			// SQL fallback: node_defs table (pre-built DBs)
			if !resolved && g.useNodesTable {
				rows, qErr := g.db.Query("SELECT node_id FROM node_defs WHERE token = ?", qualKey)
				if qErr == nil {
					for rows.Next() {
						var defID string
						if rows.Scan(&defID) == nil && defID != id && !seen[defID] {
							seen[defID] = true
							nodes = append(nodes, &Node{ID: defID, Mode: os.ModeDir | 0o555})
							resolved = true
						}
					}
					_ = rows.Close()
				}
			}
			if resolved {
				continue
			}
		}

		// Bare token lookup
		if defs != nil {
			if defIDs, ok := defs[qc.Token]; ok {
				for _, defID := range defIDs {
					if defID == id || seen[defID] {
						continue
					}
					seen[defID] = true
					nodes = append(nodes, &Node{ID: defID, Mode: os.ModeDir | 0o555})
					resolved = true
				}
				if resolved {
					continue
				}
			}
		}

		// SQL fallback: node_defs then nodes table
		if g.useNodesTable {
			var defID string
			err := g.db.QueryRow("SELECT node_id FROM node_defs WHERE token = ? LIMIT 1", qc.Token).Scan(&defID)
			if err == nil && defID != id && !seen[defID] {
				seen[defID] = true
				nodes = append(nodes, &Node{ID: defID, Mode: os.ModeDir | 0o555})
				continue
			}

			// Suffix-match fallback for receiver-method calls (bead
			// mache-9ca6af). A call like `c.sendRaw(...)` extracts as
			// the bare token "sendRaw"; node_defs has it only as the
			// qualified form "SocketClient.sendRaw". Bare lookup above
			// misses. If there's exactly ONE "*.token" entry in
			// node_defs, that's our target — unambiguous resolution
			// for the receiver case. >1 matches means ambiguous (two
			// types both define a method of this name) — skip rather
			// than guess wrongly. LIMIT 2 detects ambiguity cheaply.
			rows, qErr := g.db.Query(
				"SELECT node_id FROM node_defs WHERE token LIKE ? LIMIT 2",
				"%."+qc.Token,
			)
			if qErr == nil {
				var candidates []string
				for rows.Next() {
					var nodeID string
					if scanErr := rows.Scan(&nodeID); scanErr == nil && nodeID != id {
						candidates = append(candidates, nodeID)
					}
				}
				_ = rows.Close()
				if len(candidates) == 1 && !seen[candidates[0]] {
					seen[candidates[0]] = true
					nodes = append(nodes, &Node{ID: candidates[0], Mode: os.ModeDir | 0o555})
					continue
				}
			}

			// Final fallback: name match in nodes table
			// kind = 1 → graph.NodeKindDir (definition constructs are dirs).
			err = g.db.QueryRow("SELECT id FROM nodes WHERE name = ? AND kind = 1 LIMIT 1", qc.Token).Scan(&defID)
			if err == nil && defID != id && !seen[defID] {
				seen[defID] = true
				nodes = append(nodes, &Node{ID: defID, Mode: os.ModeDir | 0o555})
			}
		}
	}

	return nodes, nil
}
