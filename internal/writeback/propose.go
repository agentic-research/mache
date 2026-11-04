package writeback

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/leyline"
)

// A write to a node goes through leyline's propose (mache-3a0fb9): mache
// rebuilds the node's whole file from the bytes the node's offsets were taken
// from (its origin's Blob), with the node's range replaced, and proposes that
// against the same blob as its base. The daemon applies it, merges it three
// ways when the file moved since, or refuses it whole; it writes the accepted
// file into its source tree itself. mache never writes the source file.

// ErrNoBase is a write refused because mache does not hold the bytes the
// node's offsets refer to: the producer recorded no blob, or the file was
// changed by a write mache did not build (a merge) and must be re-read.
var ErrNoBase = errors.New("mache does not hold the bytes this node was read from; re-read it and write again")

// RefusedError is a proposal the daemon refused, with its typed reason.
type RefusedError struct {
	Refusal *leyline.ProposalRefusal
	Message string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("write refused (%s): %s", e.Refusal.Reason, e.Message)
}

// Applied is a write the daemon accepted.
type Applied struct {
	// Content is the node's bytes as written.
	Content []byte
	// File is the whole file mache proposed, Blob the blob the daemon now
	// holds for it. When Merged, the daemon merged the write with a change
	// made since the node was read, so its bytes are not File.
	File   []byte
	Blob   []byte
	Merged bool
}

// Propose writes newContent over origin's range through the daemon serving
// the tree mache's daemon was started on (leyline.DaemonSource), with
// origin.Blob as the base. blobs supplies that base's bytes.
func Propose(origin graph.SourceOrigin, newContent []byte, blobs graph.BlobSource) (*Applied, error) {
	if blobs == nil || origin.Blob == nil {
		return nil, ErrNoBase
	}
	base, ok := blobs.Blob(origin.Blob)
	if !ok {
		return nil, ErrNoBase
	}
	file, content, err := replaceRange(base, origin, newContent)
	if err != nil {
		return nil, err
	}
	root := leyline.DaemonSource()
	rel, err := relativeTo(root, origin.FilePath)
	if err != nil {
		return nil, err
	}
	c, err := leyline.WritableDaemon(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	res, err := c.Propose([]leyline.ProposedWrite{{Path: rel, Base: hex.EncodeToString(origin.Blob), Content: string(file)}})
	if err != nil {
		return nil, err
	}
	if res.Refusal != nil {
		return nil, &RefusedError{Refusal: res.Refusal, Message: res.Message}
	}
	return applied(res, rel, file, content)
}

// applied reads the accepted proposal's outcome for rel.
func applied(res *leyline.ProposeResult, rel string, file, content []byte) (*Applied, error) {
	i := slices.IndexFunc(res.Blobs, func(b leyline.PathHash) bool { return b.Path == rel })
	if i < 0 {
		return nil, fmt.Errorf("the daemon accepted the write to %s but named no blob for it", rel)
	}
	blob, err := hex.DecodeString(res.Blobs[i].BlobHash)
	if err != nil {
		return nil, fmt.Errorf("the daemon named blob %q for %s: %w", res.Blobs[i].BlobHash, rel, err)
	}
	return &Applied{Content: content, File: file, Blob: blob, Merged: slices.Contains(res.Merged, rel)}, nil
}

// replaceRange is base with origin's range replaced by newContent, and the
// content as it went in. A trailing newline the original range did not end
// with is dropped: agents often write through echo or a heredoc, which adds
// one, and keeping it leaves a blank-line artifact.
func replaceRange(base []byte, origin graph.SourceOrigin, newContent []byte) (file, content []byte, err error) {
	start, end := origin.StartByte, origin.EndByte
	if int(end) > len(base) || start > end {
		return nil, nil, fmt.Errorf("invalid byte range [%d:%d] for file of length %d", start, end, len(base))
	}
	if region := base[start:end]; len(region) > 0 && region[len(region)-1] != '\n' {
		newContent = bytes.TrimRight(newContent, "\n")
	}
	file = make([]byte, 0, int(start)+len(newContent)+len(base)-int(end))
	file = append(append(append(file, base[:start]...), newContent...), base[end:]...)
	return file, newContent, nil
}

// relativeTo is path relative to root, both canonical, refused when path lies
// outside root: the daemon resolves a propose's path under its --source.
func relativeTo(root, path string) (string, error) {
	r, p := leyline.CanonicalSourceRoot(root), leyline.CanonicalSourceRoot(path)
	rel, err := filepath.Rel(r, p)
	if r == "" || err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("%w: %s is not under the daemon's source %q", leyline.ErrNoWritableDaemon, path, root)
	}
	return filepath.ToSlash(rel), nil
}

// Commit records an accepted write in g: the node holds what was written, the
// file's other origins shift by the size change, and every origin in the file
// names the new blob, whose bytes blobs now remembers. After a merge the file
// is not what mache built, so its origins name no blob until it is re-read.
func Commit(g graph.Writable, nodeID string, origin graph.SourceOrigin, a *Applied, modTime time.Time) error {
	oldLen := origin.EndByte - origin.StartByte
	if delta := int32(len(a.Content)) - int32(oldLen); delta != 0 {
		g.ShiftOrigins(origin.FilePath, origin.EndByte, delta)
	}
	newOrigin := &graph.SourceOrigin{
		FilePath:  origin.FilePath,
		StartByte: origin.StartByte,
		EndByte:   origin.StartByte + uint32(len(a.Content)),
	}
	err := g.UpdateNodeContent(nodeID, a.Content, newOrigin, modTime)
	if a.Merged {
		g.SetFileBlob(origin.FilePath, nil)
		return err
	}
	if blobs := g.Blobs(); blobs != nil {
		blobs.Remember(origin.FilePath, a.Blob, a.File)
	}
	g.SetFileBlob(origin.FilePath, a.Blob)
	return err
}
