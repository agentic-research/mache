package lloschema

import "fmt"

// NodeHashExpr is the SQL expression for an occurrence row's merkle node_hash,
// for a query that reads table (_ast, node_defs or node_refs) as alias (""
// for none). ok is false when this producer writes no hash at all (mache's
// own standalone db), and the expression is then NULL.
//
// projection-v10 (leyline v0.24.0) stopped storing the 32-byte hash on the
// occurrence tables: each row carries node_content's arena-local content_id,
// and node_content.node_hash stays the identity (ley-line-open-738dd9). The
// hash is one primary-key probe away, so on v10 the expression is a
// correlated scalar lookup. Not LLO's v_ast_hash / v_node_defs_hash /
// v_node_refs_hash views: those are LEFT JOINs, and mache's views sit on the
// right of rules' LEFT JOINs, where SQLite materializes a join view whole
// (ASTSelect records 143 s against 0.2 s for the scalar form). Earlier
// projections store node_hash on the row and read it as it is.
func NodeHashExpr(qg Querier, table, alias string) (expr string, ok bool, err error) {
	col := func(c string) string {
		if alias == "" {
			return c
		}
		return alias + "." + c
	}
	stored, err := TableHasColumn(qg, table, "node_hash")
	if err != nil {
		return "", false, fmt.Errorf("probe %s.node_hash: %w", table, err)
	}
	if stored {
		return col("node_hash"), true, nil
	}
	interned, err := TableHasColumn(qg, table, "content_id")
	if err != nil {
		return "", false, fmt.Errorf("probe %s.content_id: %w", table, err)
	}
	if interned {
		return "(SELECT nc.node_hash FROM node_content nc WHERE nc.content_id = " + col("content_id") + ")", true, nil
	}
	return "NULL", false, nil
}
