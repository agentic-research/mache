package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/leyline"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func makeGetCommunitiesHandler(g graph.Graph) server.ToolHandlerFunc {
	return makeGetCommunitiesHandlerWithDone(g, nil)
}

// makeGetCommunitiesHandlerWithDone is the test-friendly variant of
// makeGetCommunitiesHandler. When pushDone is non-nil, the channel is
// closed once the fire-and-forget PushTopology goroutine returns —
// letting race-prone tests wait deterministically instead of sleeping
// or polling. Production callers use the nil-channel path via the
// public makeGetCommunitiesHandler wrapper.
func makeGetCommunitiesHandlerWithDone(g graph.Graph, pushDone chan<- struct{}) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		minSize := request.GetInt("min_size", 2)
		summary := request.GetBool("summary", false)

		rp, ok := g.(refsMapProvider)
		if !ok {
			return mcp.NewToolResultError("community detection requires a graph with cross-reference data (SQLite backend or MemoryStore with refs)"), nil
		}
		refs := rp.RefsMap()
		if len(refs) == 0 {
			type emptyResult struct {
				Communities []any   `json:"communities"`
				NumNodes    int     `json:"num_nodes"`
				NumEdges    int     `json:"num_edges"`
				Modularity  float64 `json:"modularity"`
				Message     string  `json:"message"`
			}
			data, _ := json.Marshal(emptyResult{
				Communities: []any{},
				Message: "No cross-references indexed. Community detection requires constructs that share symbols. " +
					"Ensure the source was ingested with a schema that captures references (sitter_walker or json_walker with refs). " +
					"Use get_overview to check ref_tokens count.",
			})
			return mcp.NewToolResultText(string(data)), nil
		}

		result := graph.DetectCommunities(refs, minSize)

		// Push topology to ley-line sheaf cache (fire-and-forget).
		// Errors are best-effort: the daemon may be missing
		// (DiscoverOrStart fails), unreachable (DialSocket fails),
		// or running an older protocol that doesn't support
		// sheaf_set_topology. None of those are actionable for the
		// user mid-call; log only once per process to avoid the
		// every-call spam during benchmarks and non-LLO setups.
		//
		// Snapshot the inputs PushTopology reads (Communities + each
		// community's Members) so the goroutine sees a stable view —
		// the full-output branch below mutates result.Communities in
		// place (sort + truncate slice + truncate per-community Members)
		// and would otherwise race buildRegions inside PushTopology.
		// Membership is read-only after DetectCommunities returns, so
		// reusing it without cloning is safe.
		snapComms := make([]graph.Community, len(result.Communities))
		for i, c := range result.Communities {
			snapComms[i] = graph.Community{
				ID:      c.ID,
				Members: append([]string(nil), c.Members...),
			}
		}
		snap := &graph.CommunityResult{
			Communities: snapComms,
			Membership:  result.Membership,
		}

		// Resolve the SheafInvalidator (if this graph has one) up front
		// so the goroutine below can install state atomically once it
		// successfully reaches the daemon.
		//
		// Graph backends without a watcher (control-mode lazyGraph,
		// composite mounts) don't implement sheafInvalidatorProvider —
		// the type-assert degrades silently, which is the documented
		// behavior for those modes (see buildServeGraph).
		//
		// DELIBERATELY DEFER state install until PushTopology succeeds.
		// Installing SetCommunityResult synchronously here (the prior
		// shape) paired NEW membership with the OLD sheaf/topology
		// during the dial+push window — watcher events in that window
		// looked up regions in the new membership but sent them to a
		// daemon that still held the prior topology. Fixed by atomic
		// SetState below; see PR #383 Copilot #6.
		var sip sheafInvalidatorProvider
		if sp, ok := g.(sheafInvalidatorProvider); ok {
			sip = sp
		}

		go func() {
			if pushDone != nil {
				defer close(pushDone)
			}
			sockPath, err := leyline.DiscoverOrStart()
			if err != nil {
				return // no daemon — skip silently
			}
			sock, err := leyline.DialSocket(sockPath)
			if err != nil {
				return
			}
			// Track whether we handed off socket ownership to the
			// invalidator. If we did, the SheafClient inside the
			// invalidator owns the socket and we MUST NOT close it
			// here — the watcher's later cascade calls reuse the
			// connection. If we didn't hand off, close it normally
			// to avoid leaking a socket per failed get_communities
			// run. See PR #383 Copilot #1: the prior code deferred
			// sock.Close() unconditionally, so the SheafClient
			// stored on the invalidator was talking to a closed
			// socket from the goroutine's return onward.
			var handedOff bool
			defer func() {
				if !handedOff {
					_ = sock.Close()
				}
			}()

			sc := leyline.NewSheafClient(sock)
			if pushErr := sc.PushTopology(snap, refs); pushErr != nil {
				logSheafPushOnce(pushErr)
				return
			}
			// PushTopology succeeded — atomically swap BOTH the new
			// CommunityResult and the new SheafClient onto the
			// invalidator. From here on, subsequent watcher fires
			// engage the cross-region cascade. The prior backend
			// (if any) is returned so we can close its socket and
			// avoid leaking one UDS connection per get_communities
			// call across the process lifetime.
			if sip != nil {
				if si := sip.SheafInvalidator(); si != nil {
					prior := si.SetState(snap, sc)
					handedOff = true
					if closer, ok := prior.(io.Closer); ok {
						_ = closer.Close()
					}
				}
			}
		}()

		if summary {
			type communitySummary struct {
				ID         int      `json:"id"`
				Size       int      `json:"size"`
				TopMembers []string `json:"top_members"`
			}
			type summaryResult struct {
				NumCommunities int                `json:"num_communities"`
				NumNodes       int                `json:"num_nodes"`
				NumEdges       int                `json:"num_edges"`
				Modularity     float64            `json:"modularity"`
				Communities    []communitySummary `json:"communities"`
			}
			sr := summaryResult{
				NumCommunities: len(result.Communities),
				NumNodes:       result.NumNodes,
				NumEdges:       result.NumEdges,
				Modularity:     result.Modularity,
			}
			for _, c := range result.Communities {
				top := c.Members
				if len(top) > 5 {
					top = top[:5]
				}
				// Strip trailing /source from member paths — it's noise in summaries
				cleaned := make([]string, len(top))
				for i, m := range top {
					cleaned[i] = strings.TrimSuffix(m, "/source")
				}
				sr.Communities = append(sr.Communities, communitySummary{
					ID:         c.ID,
					Size:       len(c.Members),
					TopMembers: cleaned,
				})
			}
			data, _ := json.MarshalIndent(sr, "", "  ")
			return mcp.NewToolResultText(string(data)), nil
		}

		// Non-summary full output. Bead mache-9cd921: previously
		// marshaled the full Communities slice, blowing past the MCP
		// response budget (720K chars on 28K LOC). Cap to the top N
		// largest communities + top N members within each. Truncation
		// is in-place on the result; the marshaled JSON keeps its
		// existing CommunityResult shape (PascalCase) so consumers
		// that already parse this output don't break.
		const maxCommunities = 25
		const maxMembersPerCommunity = 20

		sort.Slice(result.Communities, func(i, j int) bool {
			return len(result.Communities[i].Members) > len(result.Communities[j].Members)
		})
		eliddedCommunities := 0
		if len(result.Communities) > maxCommunities {
			eliddedCommunities = len(result.Communities) - maxCommunities
			result.Communities = result.Communities[:maxCommunities]
		}
		eliddedMembers := 0
		for i := range result.Communities {
			if len(result.Communities[i].Members) > maxMembersPerCommunity {
				eliddedMembers += len(result.Communities[i].Members) - maxMembersPerCommunity
				result.Communities[i].Members = result.Communities[i].Members[:maxMembersPerCommunity]
			}
		}

		// Anonymous wrapper embeds *CommunityResult so existing
		// consumers see the same field names; truncation metadata
		// rides as additive optional fields.
		out := struct {
			*graph.CommunityResult
			ElidedCommunities int    `json:",omitempty"`
			ElidedMembers     int    `json:",omitempty"`
			TruncationNote    string `json:",omitempty"`
		}{
			CommunityResult:   result,
			ElidedCommunities: eliddedCommunities,
			ElidedMembers:     eliddedMembers,
		}
		if eliddedCommunities > 0 || eliddedMembers > 0 {
			out.TruncationNote = fmt.Sprintf(
				"output truncated to %d communities (×%d members each) to fit MCP response budget; use summary=true for an even tighter view",
				maxCommunities, maxMembersPerCommunity,
			)
		}
		data, _ := json.MarshalIndent(out, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	}
}
