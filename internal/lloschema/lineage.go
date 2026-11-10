package lloschema

// IRLineage is the IR lineage the db's node_hash values were computed under:
// leyline stamps it as _meta.ir_schema_version (merkle-ast-v2, merkle-ast-v3,
// ...). "" when the db records none (mache's own db, or no _meta).
func IRLineage(qg Querier) (string, error) {
	has, err := TableHasColumn(qg, "_meta", "value")
	if err != nil || !has {
		return "", err
	}
	rows, err := qg.QueryRefs(`SELECT value FROM _meta WHERE key = 'ir_schema_version'`)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	var v string
	if rows.Next() {
		if err := rows.Scan(&v); err != nil {
			return "", err
		}
	}
	return v, rows.Err()
}
