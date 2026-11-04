package leyline

// Client for the daemon's read_source, propose and status ops.
//
// The wire is LLO's own: leyline-schema's daemon/wire package
// (ley-line-open-247489) decodes every response here, and the protocol fixtures
// LLO's Rust gate proves the daemon emits strict-decode into it. What this file
// adds is the parse into mache's terms: each response is checked once for the
// fields its outcome requires, so callers hold values rather than a tree of
// optional pointers.

import (
	"encoding/json"
	"fmt"

	daemonwire "github.com/agentic-research/ley-line-open/clients/go/leyline-schema/daemon/wire"
)

// ProposedWrite is one path's change in a proposal, as the wire spells it.
type ProposedWrite = daemonwire.ProposedWrite

// SourceRead is the daemon's stored copy of one file: the bytes, and the hash
// a propose names as that file's base.
type SourceRead struct {
	Content  string
	BlobHash string
	Mode     string
	TreeRoot string
}

// ReadSource reads path (relative to the daemon's --source root) as the daemon
// stores it.
func (c *SocketClient) ReadSource(path string) (*SourceRead, error) {
	// A failed read answers {"error": ...}, which SendOpInto returns as a
	// DaemonResponseError.
	var resp daemonwire.ReadSourceResponse
	if err := c.SendOpInto(map[string]any{"op": "read_source", "path": path}, &resp); err != nil {
		return nil, fmt.Errorf("leyline read_source %s: %w", path, err)
	}
	if !isTrue(resp.OK) {
		return nil, fmt.Errorf("leyline read_source %s: the daemon answered not-ok without an error", path)
	}
	if resp.Content == nil || resp.BlobHash == nil {
		return nil, fmt.Errorf("leyline read_source %s: the answer names no content or blob hash", path)
	}
	return &SourceRead{
		Content:  *resp.Content,
		BlobHash: *resp.BlobHash,
		Mode:     deref(resp.Mode),
		TreeRoot: deref(resp.TreeRoot),
	}, nil
}

// LineRange is a 0-based, half-open range of base lines.
type LineRange struct{ Start, End uint32 }

// MovedPath is a path whose base moved since it was read: the blob the write
// named, the one the store holds now, and where a merge overlapped the other
// writer's change.
type MovedPath struct {
	Path, Expected, Current string
	Conflicts               []LineRange
}

// ProposalRefusal is why a proposal was refused whole. Other is the second
// path a refusal names (path_conflict: the file Path is under;
// aliases_existing: the entry Path aliases), empty otherwise.
type ProposalRefusal struct {
	Reason      daemonwire.RefusalReason
	Path, Other string
	Moved       []MovedPath
}

// PathHash is a path and the blob it now holds.
type PathHash struct{ Path, BlobHash string }

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

// Propose applies writes as one change through the daemon's single writer, or
// refuses them whole. A refusal is a result, not an error; an error is the
// daemon unreachable, a malformed request, or an answer that is neither
// applied nor a typed refusal.
func (c *SocketClient) Propose(writes []ProposedWrite) (*ProposeResult, error) {
	// Not SendOpInto: that treats any `error` key as a failed op, and a
	// refusal carries one (the daemon's sentence) beside its typed reason.
	line, err := c.sendRaw(map[string]any{"op": "propose", "writes": writes})
	if err != nil {
		return nil, fmt.Errorf("leyline propose: %w", err)
	}
	var resp daemonwire.ProposeResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("leyline propose: unmarshal response: %w", err)
	}
	return proposeResult(&resp)
}

// proposeResult parses a propose answer: applied, refused with a reason, or an
// error. Decided on ok and refusal alone, as the wire says to, never on whether
// error is set.
func proposeResult(resp *daemonwire.ProposeResponse) (*ProposeResult, error) {
	res := &ProposeResult{
		TreeRoot:        deref(resp.TreeRoot),
		Merged:          resp.Merged,
		PostCommitError: deref(resp.PostCommitError),
		Message:         deref(resp.Error),
	}
	if !isTrue(resp.OK) {
		if resp.Refusal == nil {
			return nil, fmt.Errorf("leyline propose: %s", res.Message)
		}
		refusal, err := proposalRefusal(resp.Refusal)
		if err != nil {
			return nil, err
		}
		res.Refusal = refusal
		return res, nil
	}
	for _, b := range resp.Blobs {
		if b.Path == nil || b.BlobHash == nil {
			return nil, fmt.Errorf("leyline propose: an applied answer lists a blob without its path or hash")
		}
		res.Blobs = append(res.Blobs, PathHash{Path: *b.Path, BlobHash: *b.BlobHash})
	}
	return res, nil
}

func proposalRefusal(r *daemonwire.ProposalRefusal) (*ProposalRefusal, error) {
	if r.Reason == nil {
		return nil, fmt.Errorf("leyline propose: a refusal without a reason")
	}
	out := &ProposalRefusal{Reason: *r.Reason, Path: deref(r.Path), Other: deref(r.Other)}
	for _, m := range r.Moved {
		if m.Path == nil {
			return nil, fmt.Errorf("leyline propose: a stale refusal names a moved file without its path")
		}
		moved := MovedPath{Path: *m.Path, Expected: deref(m.Expected), Current: deref(m.Current)}
		for _, c := range m.Conflicts {
			if c.Start == nil || c.End == nil {
				return nil, fmt.Errorf("leyline propose: a conflict in %s without its lines", *m.Path)
			}
			moved.Conflicts = append(moved.Conflicts, LineRange{Start: *c.Start, End: *c.End})
		}
		out.Moved = append(out.Moved, moved)
	}
	return out, nil
}

// SourceRoot is the tree the daemon proposes into: its --source directory with
// symlinks resolved, as status reports it (ley-line-open-246526). ok is false
// for a daemon running without --source.
func (c *SocketClient) SourceRoot() (root string, ok bool, err error) {
	var resp daemonwire.StatusResponse
	if err := c.SendOpInto(map[string]any{"op": "status"}, &resp); err != nil {
		return "", false, fmt.Errorf("leyline status: %w", err)
	}
	if resp.SourceRoot == nil {
		return "", false, nil
	}
	return *resp.SourceRoot, true, nil
}

func isTrue(b *bool) bool { return b != nil && *b }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
