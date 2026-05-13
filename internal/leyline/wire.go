// Typed Go structs for the daemon JSON wire protocol.
//
// These mirror the hand-written bindings in LLO's
// `clients/go/leyline-schema/daemon/daemon_protocol_test.go` (PR #12
// commit 145fcc5). The current `leyline-schema/v0.3.0` ships those
// mirrors in `package daemon_test` (test-only), so we cannot import
// them directly — this file is the consumer-side declaration. If a
// future leyline-schema release promotes the structs to a non-test
// package, this file becomes a trivial import swap; the shapes are
// byte-identical by design.
//
// Wire encoding note: under the b0ea2e capnp-json codec (LLO ≥ v0.3.0),
// 64-bit integers ride as JSON strings to avoid JS Number precision loss.
// All Int64 / UInt64 fields carry `,string` so json.Unmarshal accepts the
// `"123"` form. Without these tags decode silently zeros the field.
// The strict-decode is intentional: pre-b0ea2e daemons emit bare numeric
// JSON which will surface as a UnmarshalTypeError rather than a silent
// wrong-zero — the failure mode is loud and points at version skew.

package leyline

// Node represents an entry in the projected graph. Used by both
// list_children (Record omitted to keep directory listings small) and
// get_node (Record present when the SQL record column is non-null).
type Node struct {
	ID       *string `json:"id"`
	ParentID *string `json:"parent_id"`
	Name     *string `json:"name"`
	Kind     *int32  `json:"kind"`
	Size     *int64  `json:"size,string"`
	Record   *string `json:"record,omitempty"`
}

// Ref is a {node_id, source_id} pair returned by find_callers /
// find_callees / find_defs. SourceID is the construct's source-file node.
type Ref struct {
	NodeID   *string `json:"node_id"`
	SourceID *string `json:"source_id"`
}

// GetNodeResponse is the response shape for op=get_node.
type GetNodeResponse struct {
	OK    *bool   `json:"ok"`
	Node  *Node   `json:"node,omitempty"`
	Error *string `json:"error,omitempty"`
}

// ListChildrenResponse is the response shape for op=list_children.
// Failure path carries Error; mache treats "not found" specially.
type ListChildrenResponse struct {
	OK       *bool   `json:"ok"`
	Children []Node  `json:"children"`
	Error    *string `json:"error,omitempty"`
}

// ReadContentResponse is the response shape for op=read_content.
type ReadContentResponse struct {
	OK      *bool   `json:"ok"`
	Content *string `json:"content,omitempty"`
	Error   *string `json:"error,omitempty"`
}

// FindCallersResponse is the response shape for op=find_callers.
type FindCallersResponse struct {
	OK      *bool   `json:"ok"`
	Callers []Ref   `json:"callers"`
	Error   *string `json:"error,omitempty"`
}

// FindCalleesResponse is the response shape for op=find_callees
// (added in LLO 0.2.2). Daemon executes the JOIN against node_refs +
// node_defs and returns the resolved construct rows.
type FindCalleesResponse struct {
	OK      *bool   `json:"ok"`
	Callees []Ref   `json:"callees"`
	Error   *string `json:"error,omitempty"`
}
