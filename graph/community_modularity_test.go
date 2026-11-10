package graph

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// computeModularity gives one bit-identical value for one partition. It used
// to sum in Go's randomized map order, so release parity of a binary against
// itself saw get_communities report modularity 0.04003286154488099 and
// 0.040032861544881 for the same communities (mache-a76e43). Weights spread
// over many magnitudes make the order show in the last bits.
func TestComputeModularity_IsBitReproducible(t *testing.T) {
	const n = 60
	adj := make([]map[int]float64, n)
	degree := make([]float64, n)
	community := make([]int, n)
	for i := range n {
		adj[i] = map[int]float64{}
		community[i] = i % 7
	}
	m2 := 0.0
	for i := range n {
		for j := range n {
			if i != j && (i*31+j*17)%5 == 0 {
				w := 1.0 / float64(1+(i*j)%97) * math.Pow(10, float64((i+j)%9-4))
				adj[i][j] += w
				degree[i] += w
				m2 += w
			}
		}
	}
	want := computeModularity(adj, community, degree, m2, n)
	for range 200 {
		require.Equal(t, math.Float64bits(want), math.Float64bits(computeModularity(adj, community, degree, m2, n)),
			"modularity changed between calls on the same partition")
	}
}
