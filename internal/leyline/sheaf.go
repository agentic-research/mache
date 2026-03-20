// Package leyline — sheaf.go provides typed methods for the ley-line daemon's
// sheaf (topology-aware cache invalidation) operations.
//
// SheafClient wraps a SocketClient and translates between mache's community
// detection output and the daemon's sheaf_* UDS ops. All methods are no-ops
// when the underlying SocketClient is nil, making the integration fully optional.
package leyline

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	graph "github.com/agentic-research/mache/internal/graph"
)

// SheafClient provides typed access to ley-line's sheaf operations.
// A nil SheafClient is safe — all methods return zero values without error.
type SheafClient struct {
	sock *SocketClient
}

// NewSheafClient wraps an existing SocketClient. sock may be nil.
func NewSheafClient(sock *SocketClient) *SheafClient {
	return &SheafClient{sock: sock}
}

// SheafStatus mirrors the response from sheaf_status / sheaf_defect.
type SheafStatus struct {
	Generation uint64  `json:"generation"`
	Valid      int     `json:"valid"`
	Total      int     `json:"total"`
	Defect     float64 `json:"defect"`
}

// region is the JSON shape sent in sheaf_set_topology.
type region struct {
	ID   int    `json:"id"`
	Hash string `json:"hash"`
}

// restriction is the JSON shape for cross-community boundary edges.
type restriction struct {
	A            int     `json:"a"`
	B            int     `json:"b"`
	BoundaryHash string  `json:"boundary_hash"`
	CoChangeRate float64 `json:"co_change_rate"`
}

// stalk is the JSON shape sent in sheaf_invalidate.
type stalk struct {
	ID   int    `json:"id"`
	Hash string `json:"hash"`
}

// PushTopology converts Louvain community detection output into a
// sheaf_set_topology op and sends it to the daemon.
//
// Each community maps to a region whose hash is the SHA-256 of the
// sorted, concatenated member node IDs. Restriction edges are derived
// from the refs map: any token referenced by nodes in different
// communities creates a boundary between those communities.
func (sc *SheafClient) PushTopology(cr *graph.CommunityResult, refs map[string][]string) error {
	if sc == nil || sc.sock == nil || cr == nil {
		return nil
	}

	regions := buildRegions(cr)
	restrictions := buildRestrictions(cr, refs)

	req := map[string]any{
		"op":           "sheaf_set_topology",
		"regions":      regions,
		"restrictions": restrictions,
	}
	resp, err := sc.sock.SendOp(req)
	if err != nil {
		return fmt.Errorf("sheaf_set_topology: %w", err)
	}
	if errMsg, ok := resp["error"]; ok {
		return fmt.Errorf("sheaf_set_topology: %v", errMsg)
	}
	return nil
}

// Invalidate marks a region as stale and returns the IDs of all regions
// that the daemon determines are transitively affected.
func (sc *SheafClient) Invalidate(regionID int) ([]int, error) {
	if sc == nil || sc.sock == nil {
		return nil, nil
	}

	req := map[string]any{
		"op":      "sheaf_invalidate",
		"regions": []int{regionID},
		"stalks": []stalk{{
			ID:   regionID,
			Hash: "", // daemon computes current hash
		}},
	}
	resp, err := sc.sock.SendOp(req)
	if err != nil {
		return nil, fmt.Errorf("sheaf_invalidate: %w", err)
	}
	if errMsg, ok := resp["error"]; ok {
		return nil, fmt.Errorf("sheaf_invalidate: %v", errMsg)
	}

	return parseIntSlice(resp["invalidated"]), nil
}

// Defect queries the global consistency defect score.
// Returns 0.0 when the daemon is unavailable.
func (sc *SheafClient) Defect() (float64, error) {
	if sc == nil || sc.sock == nil {
		return 0, nil
	}

	resp, err := sc.sock.SendOp(map[string]any{"op": "sheaf_defect"})
	if err != nil {
		return 0, fmt.Errorf("sheaf_defect: %w", err)
	}
	if errMsg, ok := resp["error"]; ok {
		return 0, fmt.Errorf("sheaf_defect: %v", errMsg)
	}

	defect, _ := resp["defect"].(float64)
	return defect, nil
}

// Status returns the full sheaf status from the daemon.
func (sc *SheafClient) Status() (SheafStatus, error) {
	if sc == nil || sc.sock == nil {
		return SheafStatus{}, nil
	}

	resp, err := sc.sock.SendOp(map[string]any{"op": "sheaf_status"})
	if err != nil {
		return SheafStatus{}, fmt.Errorf("sheaf_status: %w", err)
	}
	if errMsg, ok := resp["error"]; ok {
		return SheafStatus{}, fmt.Errorf("sheaf_status: %v", errMsg)
	}

	s := SheafStatus{}
	if v, ok := resp["generation"].(float64); ok {
		s.Generation = uint64(v)
	}
	if v, ok := resp["valid"].(float64); ok {
		s.Valid = int(v)
	}
	if v, ok := resp["total"].(float64); ok {
		s.Total = int(v)
	}
	if v, ok := resp["defect"].(float64); ok {
		s.Defect = v
	}
	return s, nil
}

// --- helpers ---

// buildRegions produces the region list for sheaf_set_topology.
// Each region's hash is SHA-256 of sorted member IDs joined by newlines.
func buildRegions(cr *graph.CommunityResult) []region {
	regions := make([]region, len(cr.Communities))
	for i, c := range cr.Communities {
		regions[i] = region{
			ID:   c.ID,
			Hash: hashMembers(c.Members),
		}
	}
	return regions
}

// buildRestrictions discovers cross-community edges from the refs map.
// For each token referenced by nodes in more than one community, we
// create a restriction edge between each pair of those communities.
// For a given (A,B) edge, boundary_hash is the SHA-256 of the sorted list
// of all tokens contributing to that edge (joined with 0-byte separators).
// The co_change_rate is proportional to how many cross-community node pairs
// share those tokens.
func buildRestrictions(cr *graph.CommunityResult, refs map[string][]string) []restriction {
	if refs == nil {
		return nil
	}

	// Collect unique edges with accumulated weight.
	type edgeKey struct{ a, b int }
	edges := map[edgeKey]float64{}
	edgeTokens := map[edgeKey][]string{}

	for token, nodeIDs := range refs {
		// Collect distinct communities for this token.
		commSet := map[int]int{} // communityID → count of members referencing this token
		for _, nid := range nodeIDs {
			if cid, ok := cr.Membership[nid]; ok {
				commSet[cid]++
			}
		}
		if len(commSet) < 2 {
			continue
		}

		// Create edges between all pairs of communities.
		comms := make([]int, 0, len(commSet))
		for c := range commSet {
			comms = append(comms, c)
		}
		sort.Ints(comms)

		for i := 0; i < len(comms); i++ {
			for j := i + 1; j < len(comms); j++ {
				key := edgeKey{comms[i], comms[j]}
				edges[key] += float64(commSet[comms[i]] * commSet[comms[j]])
				edgeTokens[key] = append(edgeTokens[key], token)
			}
		}
	}

	result := make([]restriction, 0, len(edges))
	for key, weight := range edges {
		// Normalize co_change_rate to [0, 1] using a simple sigmoid-ish cap.
		rate := weight / (weight + 1.0)

		// Boundary hash from sorted cross-community tokens.
		tokens := edgeTokens[key]
		sort.Strings(tokens)
		h := sha256.New()
		for _, t := range tokens {
			h.Write([]byte(t))
			h.Write([]byte{0})
		}

		result = append(result, restriction{
			A:            key.a,
			B:            key.b,
			BoundaryHash: hex.EncodeToString(h.Sum(nil)),
			CoChangeRate: rate,
		})
	}

	// Deterministic ordering for tests.
	sort.Slice(result, func(i, j int) bool {
		if result[i].A != result[j].A {
			return result[i].A < result[j].A
		}
		return result[i].B < result[j].B
	})

	return result
}

// hashMembers returns hex-encoded SHA-256 of sorted node IDs joined by newlines.
func hashMembers(members []string) string {
	sorted := make([]string, len(members))
	copy(sorted, members)
	sort.Strings(sorted)

	h := sha256.New()
	for i, m := range sorted {
		if i > 0 {
			h.Write([]byte("\n"))
		}
		h.Write([]byte(m))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// parseIntSlice extracts []int from a JSON-decoded []any (float64 values).
func parseIntSlice(v any) []int {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	result := make([]int, 0, len(arr))
	for _, item := range arr {
		if f, ok := item.(float64); ok {
			result = append(result, int(f))
		}
	}
	return result
}
