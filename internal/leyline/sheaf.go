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

// δ⁰ stalk geometry. Matches LLO's `real_repo_sheaf_bench.rs` shape so
// the daemon's `agreement_dim` shorthand (`project_dim_range(stalkDim,
// agreementDim)`) lines up with mache's per-region vectors without any
// extra wire translation.
//
//	data[0..agreementDim] = SHA-256 of the region's cross-community
//	  tokens (the "agreement subspace" — these dims project through
//	  restriction maps and decide whether a change crossed the boundary).
//	data[agreementDim..]  = private dims (member count, cross-degree)
//	  that don't propagate through the sheaf but keep the per-region
//	  vector distinct so the daemon can hash-discriminate stalks.
const (
	stalkDim     = 32
	agreementDim = 30
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
	ID   int       `json:"id"`
	Hash string    `json:"hash"`
	Data []float32 `json:"data,omitempty"`
}

// restriction is the JSON shape for cross-community boundary edges.
type restriction struct {
	A            int     `json:"a"`
	B            int     `json:"b"`
	BoundaryHash string  `json:"boundary_hash"`
	CoChangeRate float64 `json:"co_change_rate"`
	AgreementDim int     `json:"agreement_dim,omitempty"`
}

// stalk is the JSON shape sent in sheaf_invalidate. Hash is omitempty
// so callers that don't have a content-hash can rely on the daemon's
// "infer from data" path (see sheaf_ops.rs::op_sheaf_invalidate) rather
// than sending an empty string that would have to be re-parsed
// daemon-side as zero bytes.
type stalk struct {
	ID           int       `json:"id"`
	Hash         string    `json:"hash,omitempty"`
	Data         []float32 `json:"data,omitempty"`
	AgreementDim int       `json:"agreement_dim,omitempty"`
}

// PushTopology converts Louvain community detection output into a
// sheaf_set_topology op and sends it to the daemon.
//
// Each community maps to a region whose hash is the SHA-256 of the
// sorted, concatenated member node IDs. Each region also carries a
// 32-D f32 stalk: the first 30 dims are SHA-256 of the region's
// cross-community tokens (so two regions agree iff they share the
// same set of cross-boundary refs), and the trailing 2 dims are
// private discriminators (member count + cross-degree). Restriction
// edges carry `agreement_dim=30` so the daemon's δ⁰ check projects
// the agreement subspace.
//
// With these inputs the daemon engages δ⁰-driven invalidation
// (response advertises `delta_zero_mode: true`). Without the f32
// data the daemon silently falls back to heuristic-only mode, which
// is correct but loses the precision guarantee from LLO PR #16.
func (sc *SheafClient) PushTopology(cr *graph.CommunityResult, refs map[string][]string) error {
	if sc == nil || sc.sock == nil || cr == nil {
		return nil
	}

	crossTokens := crossCommunityTokens(cr, refs)
	regions := buildRegions(cr, crossTokens)
	restrictions := buildRestrictions(cr, refs)

	req := map[string]any{
		"op":             "sheaf_set_topology",
		"regions":        regions,
		"restrictions":   restrictions,
		"node_stalk_dim": stalkDim,
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

// Invalidate marks a region as stale without pushing new stalk data.
// The daemon's cascade falls back to the heuristic XOR-of-endpoints
// pre-filter for this op. Callers that have the post-change stalk
// available should prefer InvalidateWithStalk so the daemon's δ⁰
// check decides whether the change actually crossed the agreement
// plane.
func (sc *SheafClient) Invalidate(regionID int) ([]int, error) {
	return sc.InvalidateWithStalk(regionID, "", nil)
}

// InvalidateWithStalk reports regionID changed and pushes the new
// 32-D f32 stalk so the daemon can evaluate δ⁰ against the baseline
// recorded at sheaf_set_topology time. Returns the BFS-ordered list
// of regions the daemon determined are transitively affected.
//
// newHash may be empty — the daemon will infer the hash from data
// when both `hash` and `data` are present. When both hash and data
// are empty, this is equivalent to Invalidate.
//
// Returns an error when newData is non-empty but len(newData) is
// not stalkDim — a wrong-sized vector would either be rejected
// daemon-side with a confusing error or, worse, cause the δ⁰
// projection to silently mis-align with restriction agreement_dim
// and skip the cascade entirely.
func (sc *SheafClient) InvalidateWithStalk(regionID int, newHash string, newData []float32) ([]int, error) {
	if sc == nil || sc.sock == nil {
		return nil, nil
	}

	if len(newData) > 0 && len(newData) != stalkDim {
		return nil, fmt.Errorf("sheaf_invalidate: stalk data must be %d floats (got %d)", stalkDim, len(newData))
	}

	s := stalk{ID: regionID, Hash: newHash}
	if len(newData) > 0 {
		s.Data = newData
		s.AgreementDim = agreementDim
	}

	req := map[string]any{
		"op":      "sheaf_invalidate",
		"regions": []int{regionID},
		"stalks":  []stalk{s},
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

// ComputeStalk derives a single region's 32-D δ⁰ stalk from its
// community membership + the cross-community tokens it touches.
// Exported so the watcher path (mache-c11848) can compute a fresh
// stalk to pass to InvalidateWithStalk.
//
// The agreement-dim portion is purely a function of `crossTokens`,
// so a content change that does not alter which cross-community
// tokens the region exposes will produce the same agreement coords
// and the daemon's δ⁰ check will (correctly) skip the cascade.
func ComputeStalk(memberCount int, crossTokens []string) []float32 {
	sorted := make([]string, len(crossTokens))
	copy(sorted, crossTokens)
	sort.Strings(sorted)

	h := sha256.New()
	for _, t := range sorted {
		h.Write([]byte(t))
		h.Write([]byte{0})
	}
	digest := h.Sum(nil) // 32 bytes

	data := make([]float32, 0, stalkDim)
	for i := range agreementDim {
		data = append(data, float32(digest[i]))
	}
	// Private dims: member count + cross-degree. These never propagate
	// through the agreement subspace but keep the per-region vector
	// distinct enough that the daemon's hash-bucket discrimination
	// behaves as expected.
	data = append(data, float32(memberCount))
	data = append(data, float32(len(sorted)))
	for len(data) < stalkDim {
		data = append(data, 0.0)
	}
	return data
}

// --- helpers ---

// crossCommunityTokens groups the refs map by region: for each region
// ID, return the sorted, deduped list of tokens that appear in at
// least one member of that region AND also appear in at least one
// member of a different region. These tokens are exactly the ones
// that participate in restriction edges through the region.
func crossCommunityTokens(cr *graph.CommunityResult, refs map[string][]string) map[int][]string {
	out := make(map[int]map[string]struct{}, len(cr.Communities))
	for token, nodeIDs := range refs {
		commSet := map[int]struct{}{}
		for _, nid := range nodeIDs {
			if cid, ok := cr.Membership[nid]; ok {
				commSet[cid] = struct{}{}
			}
		}
		if len(commSet) < 2 {
			continue
		}
		for cid := range commSet {
			if out[cid] == nil {
				out[cid] = map[string]struct{}{}
			}
			out[cid][token] = struct{}{}
		}
	}

	sorted := make(map[int][]string, len(out))
	for cid, set := range out {
		tokens := make([]string, 0, len(set))
		for t := range set {
			tokens = append(tokens, t)
		}
		sort.Strings(tokens)
		sorted[cid] = tokens
	}
	return sorted
}

// buildRegions produces the region list for sheaf_set_topology.
// Each region's hash is SHA-256 of sorted member IDs joined by newlines.
// crossTokens[id] may be nil (region with no cross-boundary refs) —
// ComputeStalk handles the empty case by returning a 32-D vector
// whose agreement coords are the SHA-256 of zero input (a fixed
// "empty bucket") plus the region's private dims.
func buildRegions(cr *graph.CommunityResult, crossTokens map[int][]string) []region {
	regions := make([]region, len(cr.Communities))
	for i, c := range cr.Communities {
		regions[i] = region{
			ID:   c.ID,
			Hash: hashMembers(c.Members),
			Data: ComputeStalk(len(c.Members), crossTokens[c.ID]),
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
// share those tokens. AgreementDim is set on every edge so the daemon's
// δ⁰ check projects the matching agreement subspace.
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
			AgreementDim: agreementDim,
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
