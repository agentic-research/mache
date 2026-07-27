package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/internal/graph"
	_ "modernc.org/sqlite"
)

// seedSmellBench builds a synthetic find_smells fixture at scale.
// nDefs defines how many tokens get a def + matching node; nRefs is the
// number of distinct callers in node_refs (each pointing at a random
// def). For _ast we synthesize one source_file per "package" (10 defs
// per package) plus one function_declaration per def — enough to make
// cyclomatic_complexity non-trivial.
func seedSmellBench(b *testing.B, nDefs int) *smellTestGraph {
	b.Helper()
	dbPath := filepath.Join(b.TempDir(), "smells.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		b.Fatal(err)
	}

	if _, err := db.Exec(`
		CREATE TABLE nodes (
			id TEXT PRIMARY KEY, parent_id TEXT, name TEXT NOT NULL,
			kind INTEGER NOT NULL, size INTEGER DEFAULT 0,
			mtime INTEGER NOT NULL, record_id TEXT, record TEXT,
			source_file TEXT
		);
		CREATE TABLE _ast (
			node_id TEXT PRIMARY KEY, source_id TEXT NOT NULL,
			node_kind TEXT NOT NULL,
			start_byte INTEGER NOT NULL, end_byte INTEGER NOT NULL,
			start_row INTEGER, start_col INTEGER,
			end_row INTEGER, end_col INTEGER
		);
		CREATE TABLE _source (id TEXT PRIMARY KEY, language TEXT NOT NULL, content BLOB NOT NULL);
		CREATE TABLE node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE INDEX idx_nodes_parent ON nodes(parent_id);
		CREATE INDEX idx_node_refs_token ON node_refs(token);
	`); err != nil {
		b.Fatal(err)
	}

	tx, err := db.Begin()
	if err != nil {
		b.Fatal(err)
	}

	// One source_file per 10 defs; gives _ast a believable shape.
	pkgSize := 10
	for i := range nDefs {
		token := fmt.Sprintf("Func%05d", i)
		nodeID := fmt.Sprintf("pkg/%d/%s", i/pkgSize, token)
		sourceID := fmt.Sprintf("pkg/%d/file.go", i/pkgSize)

		if _, err := tx.Exec(
			"INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES (?, ?, ?, 1, 0, ?, '')",
			nodeID, fmt.Sprintf("pkg/%d", i/pkgSize), token, sourceID,
		); err != nil {
			b.Fatal(err)
		}
		if _, err := tx.Exec("INSERT INTO node_defs VALUES (?, ?)", token, nodeID); err != nil {
			b.Fatal(err)
		}
		// _ast: one function_declaration per def with a few branches.
		if _, err := tx.Exec(
			"INSERT INTO _ast (node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col) VALUES (?, ?, 'function_declaration', ?, ?, ?, ?, ?, 0)",
			nodeID, sourceID, i*1000, i*1000+400, i*30, 0, i*30+25,
		); err != nil {
			b.Fatal(err)
		}
		// Three branches per function — hits cyclomatic_complexity sweet spot.
		for j, kind := range []string{"if_statement", "for_statement", "case_clause"} {
			if _, err := tx.Exec(
				"INSERT INTO _ast (node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col) VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0)",
				fmt.Sprintf("%s/branch_%d", nodeID, j), sourceID, kind, i*1000+j*50, i*1000+j*50+30, i*30+j*5, 0,
			); err != nil {
				b.Fatal(err)
			}
		}

		// 60% of defs get one ref so dead_code finds the other 40%.
		if i%5 < 3 {
			caller := fmt.Sprintf("pkg/%d/Caller%05d/source", i/pkgSize, i)
			if _, err := tx.Exec("INSERT INTO node_refs VALUES (?, ?)", token, caller); err != nil {
				b.Fatal(err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	return &smellTestGraph{MemoryStore: graph.NewMemoryStore(), db: db}
}

// BenchmarkFindSmells_DeadCode exercises the node_defs/node_refs path,
// which is the rule with the largest scan surface for standalone mache
// (no _ast required).
func BenchmarkFindSmells_DeadCode(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("defs=%d", n), func(b *testing.B) {
			tg := seedSmellBench(b, n)
			defer func() { _ = tg.db.Close() }()
			handler := makeFindSmellsHandler(tg)
			req := makeRequest(map[string]any{"rule": "dead_code", "limit": float64(10000)})
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, err := handler(context.Background(), req)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkFindSmells_CyclomaticComplexity exercises the _ast self-join
// path. Worth measuring separately because the LIKE prefix join is the
// expensive operation in metric-style rules.
func BenchmarkFindSmells_CyclomaticComplexity(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("defs=%d", n), func(b *testing.B) {
			tg := seedSmellBench(b, n)
			defer func() { _ = tg.db.Close() }()
			handler := makeFindSmellsHandler(tg)
			req := makeRequest(map[string]any{
				"rule":       "cyclomatic_complexity",
				"limit":      float64(10000),
				"min_metric": float64(1),
			})
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, err := handler(context.Background(), req)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkFindSmells_FanOutSkew exercises the CTE-with-aggregate path.
// Caller distribution dominates the cost (one row per distinct caller
// in node_refs).
func BenchmarkFindSmells_FanOutSkew(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("defs=%d", n), func(b *testing.B) {
			tg := seedSmellBench(b, n)
			defer func() { _ = tg.db.Close() }()
			handler := makeFindSmellsHandler(tg)
			req := makeRequest(map[string]any{"rule": "fan_out_skew", "limit": float64(10000)})
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, err := handler(context.Background(), req)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkFindSmells_RulesListing measures the discovery-mode call —
// no rule arg, just returns the registry. This is the hot path for
// agents pre-flighting the tool against a backend.
func BenchmarkFindSmells_RulesListing(b *testing.B) {
	tg := seedSmellBench(b, 0)
	defer func() { _ = tg.db.Close() }()
	handler := makeFindSmellsHandler(tg)
	req := makeRequest(map[string]any{})
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := handler(context.Background(), req)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkFindSmells_UntestedFunction exercises the rule's two
// LEFT JOINs and the tested_via_call CTE. Worth tracking because
// each PR in this session added a new clause: TestType* prefix
// (PR #250), tested_via_call CTE (PR #251), Register* skip (PR
// #273). Cumulative cost on a 10k-defs fixture is the regression
// signal we care about.
func BenchmarkFindSmells_UntestedFunction(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("defs=%d", n), func(b *testing.B) {
			tg := seedSmellBench(b, n)
			defer func() { _ = tg.db.Close() }()
			handler := makeFindSmellsHandler(tg)
			req := makeRequest(map[string]any{"rule": "untested_function", "limit": float64(10000)})
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, err := handler(context.Background(), req)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkFindSmells_DuplicateDefinitions exercises the inner
// GROUP BY token + outer JOIN node_defs pattern. Cost scales with
// distinct-tokens × duplicate-count.
func BenchmarkFindSmells_DuplicateDefinitions(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("defs=%d", n), func(b *testing.B) {
			tg := seedSmellBench(b, n)
			defer func() { _ = tg.db.Close() }()
			handler := makeFindSmellsHandler(tg)
			req := makeRequest(map[string]any{"rule": "duplicate_definitions", "limit": float64(10000)})
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, err := handler(context.Background(), req)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkFindSmells_GodFile exercises the per_file aggregate +
// project-mean CROSS JOIN pattern. Cost dominated by the CTE
// scanning all node_defs once (tracked separately from
// duplicate_definitions because the JOIN shape differs).
func BenchmarkFindSmells_GodFile(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("defs=%d", n), func(b *testing.B) {
			tg := seedSmellBench(b, n)
			defer func() { _ = tg.db.Close() }()
			handler := makeFindSmellsHandler(tg)
			req := makeRequest(map[string]any{"rule": "god_file", "limit": float64(10000)})
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, err := handler(context.Background(), req)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
