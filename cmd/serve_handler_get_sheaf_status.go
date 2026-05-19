package cmd

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/agentic-research/mache/internal/leyline"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// makeGetSheafStatusHandler returns the handler for the get_sheaf_status
// MCP tool. The tool surfaces TWO pieces of state to agents:
//
//  1. The daemon's sheaf cache state (generation, valid/total entries,
//     defect score) — same shape PR #383 shipped.
//  2. The local subscriber's state (mache-c14c43): whether mache is
//     receiving sheaf.invalidate events, the last event seen, and the
//     last generation observed in an event. Lets agents distinguish
//     "daemon is at gen 7" (queried fresh) from "mache has SEEN gen 7
//     pushed to it" (subscriber is actually live). Without the second,
//     an agent could see a stale generation and not know whether
//     mache's caches have been flushed.
//
// Design contract: the handler MUST NOT surface daemon unavailability
// as an MCP error. Agents polling this tool would otherwise see
// transport failures whenever the daemon is down or hasn't been
// dialed yet. Instead, return a structured {available: false, reason:
// "..."} response. This matches the documented graceful-degradation
// pattern in the wider sheaf wiring (cmd/serve.go).
//
// DiscoverSocket (not DiscoverOrStart) is the right primitive here:
// a status check should never trigger a daemon auto-spawn (which can
// download the binary, take seconds, etc.).
//
// subscriberStatus is the closure from graphRegistry.sheafSubscriberAccessor.
// When nil (called from a context without a registry — only the
// regression test for the registered-tools-set), the response omits
// the subscriber fields entirely rather than synthesizing a fake
// "not subscribed" reading.
func makeGetSheafStatusHandler(subscriberStatus func() (leyline.SubscriberStatus, bool)) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Snapshot the subscriber up front. We surface it on every
		// response shape — even {available: false} — so agents can
		// tell "no daemon at all" vs "daemon down but subscriber
		// reconnecting" without parsing the reason string.
		var subState *leyline.SubscriberStatus
		if subscriberStatus != nil {
			if st, ok := subscriberStatus(); ok {
				subState = &st
			}
		}

		unavailable := func(reason string) (*mcp.CallToolResult, error) {
			body := map[string]any{
				"available": false,
				"reason":    reason,
			}
			if subState != nil { // coverage:ignore — defensive guard; reduction tracked in mache-89b5dd.
				body["subscriber"] = subscriberFieldsFor(*subState) // coverage:ignore — defensive guard; reduction tracked in mache-89b5dd.
			} // coverage:ignore — defensive guard; reduction tracked in mache-89b5dd.
			data, _ := json.Marshal(body)
			return mcp.NewToolResultText(string(data)), nil
		}

		sockPath, err := leyline.DiscoverSocket()
		if err != nil {
			return unavailable("no ley-line daemon socket — set LEYLINE_SOCKET or start `leyline daemon`")
		}
		sock, err := leyline.DialSocket(sockPath)
		if err != nil {
			return unavailable(fmt.Sprintf("dial %s: %v", sockPath, err))
		}
		defer func() { _ = sock.Close() }()

		sc := leyline.NewSheafClient(sock)
		s, err := sc.Status()
		if err != nil {
			return unavailable(fmt.Sprintf("sheaf_status: %v", err))
		}

		body := map[string]any{
			"available":  true,
			"generation": s.Generation,
			"valid":      s.Valid,
			"total":      s.Total,
			"defect":     s.Defect,
		}
		if subState != nil {
			body["subscriber"] = subscriberFieldsFor(*subState)
		}
		data, _ := json.Marshal(body)
		return mcp.NewToolResultText(string(data)), nil
	}
}

// subscriberFieldsFor renders a SubscriberStatus into the MCP wire
// shape. Kept out of the handler body so the field names + lower-case
// state string are a single read away from the regression test that
// pins them.
func subscriberFieldsFor(s leyline.SubscriberStatus) map[string]any {
	out := map[string]any{
		"state":           s.State.String(),
		"last_generation": s.LastGeneration,
	}
	if s.Reason != "" {
		out["reason"] = s.Reason
	}
	if !s.LastEvent.IsZero() {
		out["last_event_unix"] = s.LastEvent.Unix()
	}
	return out
}
