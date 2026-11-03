package lloschema

// SourceBytesExpr is the SQL expression for a `_source` row's bytes on qg, for
// a query that reads `_source` under its own name (no alias).
//
// leyline leaves `_source.content` NULL and keeps the parsed bytes in
// `source_blobs`, keyed by `_source.content_hash` (every release from v0.18.2
// on; mache-46c59d). Those are the bytes every node's offsets refer to, so they
// are the only correct source text: the file on disk may have changed since
// the parse. A row that carries its bytes inline, as mache's cache-hydrated
// db writes them, is read as it is. A db with no `source_blobs` table can only
// answer inline, and naming the table there would fail the whole statement,
// so the expression is chosen per db. A row with neither reads as NULL, which
// callers refuse; nothing falls back to reading the file.
func SourceBytesExpr(qg Querier) (string, error) {
	hasBlobs, err := TableHasColumn(qg, "source_blobs", "blob_bytes")
	if err != nil {
		return "", err
	}
	if !hasBlobs {
		return "_source.content", nil
	}
	return "COALESCE(_source.content, (SELECT source_blobs.blob_bytes FROM source_blobs " +
		"WHERE source_blobs.blob_hash = _source.content_hash))", nil
}
