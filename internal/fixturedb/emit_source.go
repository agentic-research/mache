package fixturedb

import "crypto/sha256"

// Writing `_source` rows the way each producer writes them (mache-46c59d).

// insertV4Sources writes the projection-v4 `_source` rows. leyline's own
// (blobs) carry their bytes as leyline does, in source_blobs under
// content_hash with content NULL; mache's cache-hydrated db (Standalone)
// carries them inline, as buildcache writes them.
func (e *emitter) insertV4Sources(blobs bool) {
	for _, id := range e.b.srcOrder {
		s := e.b.sources[id]
		body, hash := e.sourceBytes(s, blobs)
		e.exec(`INSERT OR REPLACE INTO _source (id, language, content, path, content_hash)
			VALUES (?, ?, ?, ?, ?)`, string(s.id), s.lang, body, s.path, hash)
	}
}

// sourceBytes is what a `_source` row carries for s: inline content, or (blobs)
// NULL content and the hash of a source_blobs row holding the bytes. A source
// declared without content carries neither.
func (e *emitter) sourceBytes(s *sourceSpec, blobs bool) (body, hash any) {
	if s.content == "" {
		return nil, nil
	}
	if !blobs {
		return []byte(s.content), nil
	}
	return nil, storeBlob(e.exec, s.content)
}

// insertV6Sources writes the projection-v6 `_source` rows, each with the
// interned file id its nodes are keyed under.
func (e *emitter) insertV6Sources() {
	t := e.v6Tree()
	for _, id := range e.b.srcOrder {
		s := e.b.sources[id]
		body, hash := e.sourceBytes(s, true)
		e.exec(`INSERT OR REPLACE INTO _source (id, language, content, path, content_hash, file_id)
			VALUES (?, ?, ?, ?, ?, ?)`, string(s.id), s.lang, body, s.path, hash, t.byPath[string(s.id)].fileID)
	}
}

// storeBlob writes content's source_blobs row through exec and returns its
// hash: SHA-256 of the bytes. ley-line's own hash is over its canonical stream
// and mache never recomputes it, so a fixture needs only a stable key.
func storeBlob(exec func(string, ...any), content string) []byte {
	h := sha256.Sum256([]byte(content))
	exec(`INSERT OR IGNORE INTO source_blobs (blob_hash, blob_bytes) VALUES (?, ?)`, h[:], []byte(content))
	return h[:]
}
