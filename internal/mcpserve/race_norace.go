//go:build !race

package mcpserve

// raceEnabled is true when the binary was built with `go test -race` /
// `go build -race`. The companion file race_race.go provides the
// true-valued variant. Perf-bearing tests use this to skip themselves
// under the race detector, where SQLite-heavy workloads run 10-20×
// slower and any fixed wall-clock baseline is meaningless.
const raceEnabled = false
