package ingest

import (
	"fmt"
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

func PrintMemUsage() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	fmt.Printf("Alloc = %v MiB\n", bToMb(m.Alloc))
	fmt.Printf("\tTotalAlloc = %v MiB\n", bToMb(m.TotalAlloc))
	fmt.Printf("\tSys = %v MiB\n", bToMb(m.Sys))
	fmt.Printf("\tNumGC = %v\n", m.NumGC)
}

func bToMb(b uint64) uint64 {
	return b / 1024 / 1024
}
