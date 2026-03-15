package ingest

import (
	"runtime"
	"testing"

	"github.com/agentic-research/mache/api"
)

func BenchmarkJsonWalker_Query(b *testing.B) {
	walker := NewJsonWalker()
	data := map[string]any{
		"cve": map[string]any{
			"id": "CVE-2024-1234",
			"metadata": map[string]any{
				"published": "2024-01-01",
			},
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = walker.Query(data, "$.cve")
	}
}

func BenchmarkEngine_ProcessRecord(b *testing.B) {
	schema := &api.Topology{
		Nodes: []api.Node{
			{
				Name:     "{{.cve.id}}",
				Selector: "$.cve",
				Children: []api.Node{
					{
						Name:     "metadata.json",
						Selector: "$",
						Files: []api.Leaf{
							{
								Name:            "metadata.json",
								ContentTemplate: "{{json .metadata}}",
							},
						},
					},
				},
			},
		},
	}

	walker := NewJsonWalker()
	rawJSON := `{"cve": {"id": "CVE-2024-1234", "metadata": {"published": "2024-01-01"}}}`
	job := recordJob{recordID: "1", raw: rawJSON}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = processRecord(schema, walker, "db.sqlite", job)
	}
}

func BenchmarkEngine_Parallelism(b *testing.B) {
	// Simulate parallel ingestion workload
	schema := &api.Topology{
		Nodes: []api.Node{
			{
				Name:     "{{.cve.id}}",
				Selector: "$.cve",
			},
		},
	}

	walker := NewJsonWalker()
	rawJSON := `{"cve": {"id": "CVE-2024-1234"}}`
	job := recordJob{recordID: "1", raw: rawJSON}

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = processRecord(schema, walker, "db.sqlite", job)
		}
	})
}

func PrintMemUsage(tb testing.TB) {
	tb.Helper()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	tb.Logf("Alloc = %v MiB", bToMb(m.Alloc))
	tb.Logf("\tTotalAlloc = %v MiB", bToMb(m.TotalAlloc))
	tb.Logf("\tSys = %v MiB", bToMb(m.Sys))
	tb.Logf("\tNumGC = %v", m.NumGC)
}

func bToMb(b uint64) uint64 {
	return b / 1024 / 1024
}
