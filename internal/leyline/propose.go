package leyline

// Client for the daemon's read_source and propose ops (ley-line-open >=
// v0.21.0), the write path mache-3a0fb9 moves onto.
//
// The wire is LLO's public schema, rs/ll-core/public-schema/capnp/daemon.capnp
// at the pin (ReadSourceRequest/Response, ProposedWrite, ProposeRequest/
// Response, ProposalRefusal, StaleRefusal, PathHash, LineRange), spelled as its
// $Json.name annotations say. leyline-schema's Go client carries the capnp
// types but no JSON form of them, so these structs mirror the JSON, and
// TestProposeWire_AgainstThePinnedDaemon checks them against the real binary.

import (
	"encoding/json"
	"fmt"
)

// SourceRead is the daemon's stored copy of one file: the bytes, and the hash
// a propose names as that file's base.
type SourceRead struct {
	Content  string `json:"content"`
	BlobHash string `json:"blob_hash"`
	Mode     string `json:"mode"`
	TreeRoot string `json:"tree_root"`
}

type readSourceResponse struct {
	SourceRead
	OK bool `json:"ok"`
}

// ReadSource reads path (relative to the daemon's --source root) as the daemon
// stores it.
func (c *SocketClient) ReadSource(path string) (*SourceRead, error) {
	// A failed read answers {"error": ...}, which SendOpInto returns as a
	// DaemonResponseError.
	var resp readSourceResponse
	if err := c.SendOpInto(map[string]any{"op": "read_source", "path": path}, &resp); err != nil {
		return nil, fmt.Errorf("leyline read_source %s: %w", path, err)
	}
	if !resp.OK {
		return nil, fmt.Errorf("leyline read_source %s: the daemon answered not-ok without an error", path)
	}
	return &resp.SourceRead, nil
}

// WriteKind is ProposedWrite.kind. An unset kind is replace.
type WriteKind string

const (
	WriteReplace WriteKind = "replace"
	WriteCreate  WriteKind = "create"
	WriteDelete  WriteKind = "delete"
	WriteMkdir   WriteKind = "mkdir"
	WriteRmdir   WriteKind = "rmdir"
)

// Bases that name no blob: BaseAbsent for a create or mkdir, BaseAny to write
// without a check. Otherwise a base is a 64-hex blob hash (SourceRead.BlobHash,
// or a projection's _source.content_hash).
const (
	BaseAbsent = "absent"
	BaseAny    = "any"
)

// ProposedWrite is one path's change in a proposal.
type ProposedWrite struct {
	Path    string    `json:"path"`
	Base    string    `json:"base"`
	Kind    WriteKind `json:"kind,omitempty"`
	Content string    `json:"content,omitempty"`
	Mode    string    `json:"mode,omitempty"`
}

// RefusalReason is ProposalRefusal.reason, as LLO's schema spells it.
type RefusalReason string

const (
	RefusalStale         RefusalReason = "stale"
	RefusalNoSuchFile    RefusalReason = "no_such_file"
	RefusalMalformedBase RefusalReason = "malformed_base"
	RefusalExists        RefusalReason = "exists"
)

// LineRange is a 0-based, half-open range of base lines.
type LineRange struct {
	Start uint32 `json:"start"`
	End   uint32 `json:"end"`
}

// MovedPath is a path whose base moved since it was read: the blob the write
// named, the one the store holds now, and where a merge overlapped the other
// writer's change.
type MovedPath struct {
	Path      string      `json:"path"`
	Expected  string      `json:"expected"`
	Current   string      `json:"current"`
	Conflicts []LineRange `json:"conflicts"`
}

// ProposalRefusal is why a proposal was refused whole.
type ProposalRefusal struct {
	Reason RefusalReason `json:"reason"`
	Path   string        `json:"path"`
	Moved  []MovedPath   `json:"moved"`
}

// PathHash is a path and the blob it now holds.
type PathHash struct {
	Path     string `json:"path"`
	BlobHash string `json:"blob_hash"`
}

// ProposeResult is a proposal's outcome. Applied: Refusal is nil and Blobs
// names each written path's new blob; Merged lists the paths whose base had
// moved and were merged three ways, so their bytes are not the ones proposed.
// Refused: Refusal says why, and Message is the daemon's sentence for it.
// PostCommitError is a step after the commit that failed; the proposal is
// still applied.
type ProposeResult struct {
	TreeRoot        string
	Blobs           []PathHash
	Merged          []string
	PostCommitError string
	Refusal         *ProposalRefusal
	Message         string
}

type proposeResponse struct {
	OK              bool             `json:"ok"`
	TreeRoot        string           `json:"tree_root"`
	Blobs           []PathHash       `json:"blobs"`
	Refusal         *ProposalRefusal `json:"refusal"`
	Error           string           `json:"error"`
	PostCommitError string           `json:"post_commit_error"`
	Merged          []string         `json:"merged"`
}

// Propose applies writes as one change through the daemon's single writer, or
// refuses them whole. A refusal is a result, not an error; an error is the
// daemon unreachable, a malformed request, or a not-ok answer with no typed
// refusal.
func (c *SocketClient) Propose(writes []ProposedWrite) (*ProposeResult, error) {
	// Not SendOpInto: that treats any `error` key as a failed op, and a
	// refusal carries one (the daemon's sentence) beside its typed reason.
	line, err := c.sendRaw(map[string]any{"op": "propose", "writes": writes})
	if err != nil {
		return nil, fmt.Errorf("leyline propose: %w", err)
	}
	var resp proposeResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("leyline propose: unmarshal response: %w", err)
	}
	if !resp.OK && resp.Refusal == nil {
		return nil, fmt.Errorf("leyline propose: %s", resp.Error)
	}
	return &ProposeResult{
		TreeRoot:        resp.TreeRoot,
		Blobs:           resp.Blobs,
		Merged:          resp.Merged,
		PostCommitError: resp.PostCommitError,
		Refusal:         resp.Refusal,
		Message:         resp.Error,
	}, nil
}
