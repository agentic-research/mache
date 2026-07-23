package leyline

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// BenchmarkCascade_InvalidateWithStalk measures the wall-clock latency
// of one `sheaf_invalidate` round-trip against a live ley-line daemon,
// with full δ⁰ inputs (32-D f32 stalk + agreement_dim=30).
//
// This is the moat number. The audit claim is "edit → fresh-result
// <500ms"; the single cascade round-trip is the irreducible kernel of
// that budget. Everything else (watcher debounce, ReIngestFile, MCP
// cache flush) is on top.
//
// Reads:
//
//	BenchmarkCascade_InvalidateWithStalk-N    K  μs/op  B/op  allocs/op
//
// Where N is GOMAXPROCS, K is the number of iterations, and μs/op is
// the typical metric — the daemon round-trip is dominated by JSON
// marshal/unmarshal + UDS write/read, neither of which scales with
// CPU count.
//
// Skipped automatically when the pinned `leyline` cannot be resolved (same
// gate as TestE2E_SheafCascade_AgainstLiveDaemon).
func BenchmarkCascade_InvalidateWithStalk(b *testing.B) {
	leylineBin, err := ResolveBinary(false)
	if err != nil {
		b.Skipf("pinned leyline unavailable — skipping live-daemon bench: %v", err)
	}

	sock, cleanup := startDaemonForBench(b, leylineBin)
	defer cleanup()

	sc := NewSheafClient(sock)

	// Push a synthetic 4-region topology with δ⁰ inputs engaged.
	// Same shape as TestE2E_SheafCascade_AgainstLiveDaemon so the
	// numbers are comparable to the e2e test's wall-clock.
	regions := []region{
		{ID: 1, Hash: "aaaaaaaa", Data: bench32D(1.0)},
		{ID: 2, Hash: "bbbbbbbb", Data: bench32D(2.0)},
		{ID: 3, Hash: "cccccccc", Data: bench32D(3.0)},
		{ID: 4, Hash: "dddddddd", Data: bench32D(4.0)},
	}
	restrictions := []restriction{
		{A: 1, B: 2, BoundaryHash: "ab", CoChangeRate: 0.5, AgreementDim: agreementDim},
		{A: 2, B: 3, BoundaryHash: "bc", CoChangeRate: 0.5, AgreementDim: agreementDim},
		{A: 3, B: 4, BoundaryHash: "cd", CoChangeRate: 0.5, AgreementDim: agreementDim},
	}
	require.NoError(b, pushTopologyForBench(sc, regions, restrictions))

	stalkData := bench32D(99.0) // mutated payload — daemon's δ⁰ check fires

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Rotate the region under test so we don't repeatedly invalidate
		// the same region (which the daemon's cache might short-circuit).
		region := (i % 4) + 1
		_, err := sc.InvalidateWithStalk(region, "iter", stalkData)
		if err != nil {
			b.Fatalf("InvalidateWithStalk: %v", err)
		}
	}
	b.StopTimer()

	// Report ns/op explicitly — go test -benchmem will fill in B/op
	// and allocs/op. Also report the µs/op value rounded for the eye.
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/1e3, "µs/op-wall")
}

// BenchmarkCascade_InvalidateHeuristicMode measures the same round-
// trip but WITHOUT f32 stalk data — daemon falls back to heuristic
// hash-only invalidation. Pairs with BenchmarkCascade_InvalidateWithStalk
// so the cost of the δ⁰ path is visible: with stalks we get the
// precision win (per LLO's falsifiability gates), but does the wire
// cost differ enough to matter?
//
// On 32-byte stalks the answer is "barely" — the JSON payload is ~200
// bytes larger, dominated by the float64 encoding of 32 f32 values.
// Worth measuring so any future regression that bloats the wire format
// is caught.
func BenchmarkCascade_InvalidateHeuristicMode(b *testing.B) {
	leylineBin, err := ResolveBinary(false)
	if err != nil {
		b.Skipf("pinned leyline unavailable — skipping live-daemon bench: %v", err)
	}

	sock, cleanup := startDaemonForBench(b, leylineBin)
	defer cleanup()

	sc := NewSheafClient(sock)

	// Push topology WITHOUT data + agreement_dim — daemon falls back
	// to heuristic-only (delta_zero_mode: false in the response).
	regions := []region{
		{ID: 1, Hash: "aa"}, {ID: 2, Hash: "bb"}, {ID: 3, Hash: "cc"}, {ID: 4, Hash: "dd"},
	}
	restrictions := []restriction{
		{A: 1, B: 2, BoundaryHash: "ab", CoChangeRate: 0.5},
		{A: 2, B: 3, BoundaryHash: "bc", CoChangeRate: 0.5},
		{A: 3, B: 4, BoundaryHash: "cd", CoChangeRate: 0.5},
	}
	require.NoError(b, pushTopologyForBench(sc, regions, restrictions))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := sc.Invalidate((i % 4) + 1)
		if err != nil {
			b.Fatalf("Invalidate: %v", err)
		}
	}
	b.StopTimer()

	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/1e3, "µs/op-wall")
}

// startDaemonForBench spawns a real leyline daemon in a tempdir and
// returns a dialed SocketClient + a cleanup func. Bench equivalent of
// the helper in sheaf_e2e_test.go — the *bench* version because the
// e2e helpers use *testing.T which the bench can't pass in.
//
// SUN_LEN avoidance: leyline derives the UDS socket path from --control
// (replaces .ctrl with .sock); macOS caps sun_path at ~104 bytes, so
// the entire daemon-state dir lives under /tmp/sheaf-bench-XXX rather
// than t.TempDir() (which gives /var/folders/h3/...).
func startDaemonForBench(b *testing.B, leylineBin string) (*SocketClient, func()) {
	b.Helper()
	tdir, err := os.MkdirTemp("/tmp", "sheaf-bench-")
	require.NoError(b, err)

	arena := filepath.Join(tdir, "arena.bin")
	ctrl := filepath.Join(tdir, "test.ctrl")
	sockPath := filepath.Join(tdir, "test.sock")

	daemon := exec.Command(leylineBin, "daemon",
		"--arena", arena,
		"--control", ctrl,
		"--timeout", "5m",
	)
	logFile, _ := os.Create(filepath.Join(tdir, "daemon.log"))
	daemon.Stdout = logFile
	daemon.Stderr = logFile
	require.NoError(b, daemon.Start())

	// Wait for socket to appear.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	sock, err := DialSocket(sockPath)
	if err != nil {
		_ = daemon.Process.Kill()
		_ = os.RemoveAll(tdir)
		b.Fatalf("dial bench daemon: %v", err)
	}

	return sock, func() {
		_ = sock.Close()
		if daemon.Process != nil {
			_ = daemon.Process.Signal(syscall.SIGTERM)
			done := make(chan struct{})
			go func() {
				_, _ = daemon.Process.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				_ = daemon.Process.Kill()
				<-done
			}
		}
		_ = logFile.Close()
		_ = os.RemoveAll(tdir)
	}
}

// pushTopologyForBench sends sheaf_set_topology with the given regions
// + restrictions. Bench helper for the live-daemon benches above —
// avoids depending on SheafClient.PushTopology, which only accepts a
// graph.CommunityResult (forces the bench to construct a fake one).
func pushTopologyForBench(sc *SheafClient, regs []region, rests []restriction) error {
	req := map[string]any{
		"op":             "sheaf_set_topology",
		"regions":        regs,
		"restrictions":   rests,
		"node_stalk_dim": stalkDim,
	}
	resp, err := sc.sock.SendOp(req)
	if err != nil {
		return err
	}
	if e, ok := resp["error"]; ok {
		return &topologyError{msg: toStr(e)}
	}
	return nil
}

type topologyError struct{ msg string }

func (e *topologyError) Error() string { return "sheaf_set_topology: " + e.msg }

func toStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// bench32D returns a 32-D f32 stalk with values seeded by f for
// uniqueness. Mirrors ComputeStalk's shape so the bench data is
// realistic without invoking the SHA-256 path on every iteration.
func bench32D(f float32) []float32 {
	v := make([]float32, stalkDim)
	for i := range v {
		v[i] = f + float32(i)*0.01
	}
	return v
}

// Avoid an unused-import warning on platforms where the bench is the
// only consumer of `strings`. Kept inline so a future refactor of the
// helpers above can drop the import without an unrelated diff in the
// bench file.
var _ = strings.HasPrefix
