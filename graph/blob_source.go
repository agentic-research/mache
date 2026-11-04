package graph

import "time"

// BlobSource holds the bytes of the parsed blobs a store's origins name, so a
// write can rebuild the whole file its node sits in from exactly the bytes the
// node's offsets were taken from (SourceOrigin.Blob), never from the file on
// disk (mache-3a0fb9).
type BlobSource interface {
	// Blob returns the bytes hash names, and false when the source does not
	// hold them.
	Blob(hash []byte) ([]byte, bool)
	// Remember records content under hash as filePath's bytes: what a write
	// produced and the daemon accepted, which the next write to that file
	// builds on. It replaces what was remembered for filePath before.
	Remember(filePath string, hash, content []byte)
}

// SetBlobSource configures the store's BlobSource. Like SetCallExtractor, it
// is set once, where the store's projection is built, before it serves.
func (s *MemoryStore) SetBlobSource(b BlobSource) { s.blobs = b }

// Blobs returns the store's BlobSource, nil when none is configured.
func (s *MemoryStore) Blobs() BlobSource { return s.blobs }

// SetFileBlob names hash as the blob every origin in filePath refers to: after
// a write the daemon accepted, the file is those bytes. nil marks the file's
// origins as naming no blob, after a write whose result mache did not build
// (a merge), so the next write is refused until the file is re-read.
func (s *MemoryStore) SetFileBlob(filePath string, hash []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bm, ok := s.fileToNodes[filePath]
	if !ok {
		return
	}
	it := bm.Iterator()
	for it.HasNext() {
		intID := it.Next()
		if int(intID) >= len(s.intToNodeID) {
			continue
		}
		if n, ok := s.nodes[s.intToNodeID[intID]]; ok && n.Origin != nil && n.Origin.FilePath == filePath {
			n.Origin.Blob = hash
		}
	}
}

// Writable is a graph a write can be committed to (writeback.Commit).
type Writable interface {
	UpdateNodeContent(id string, data []byte, origin *SourceOrigin, modTime time.Time) error
	ShiftOrigins(filePath string, afterByte uint32, delta int32)
	SetFileBlob(filePath string, hash []byte)
	Blobs() BlobSource
}

var _ Writable = (*MemoryStore)(nil)
