package smells

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Independent CLI processes may overlap on the same database. RunFindSmells
// deliberately shares command flag globals, so do not call it concurrently
// within one process to model CLI invocations.
func TestFindSmellsCLI_OverlappingProcesses(t *testing.T) {
	if path := os.Getenv("MACHE_TEST_SMELL_CLI_DB"); path != "" {
		code, err := RunFindSmells(FindSmellsOptions{DB: path, Rule: "dead_code", Format: "json", Limit: 200, Out: os.Stdout, Err: os.Stderr})
		if err != nil {
			os.Exit(99)
		}
		os.Exit(code)
	}
	g := pooledSmellFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	run := func() ([]byte, error) {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFindSmellsCLI_OverlappingProcesses$")
		cmd.Env = append(os.Environ(), "MACHE_TEST_SMELL_CLI_DB="+g.Path)
		return cmd.CombinedOutput()
	}
	want, err := run()
	require.NoError(t, err, "%s", want)
	require.Contains(t, string(want), "pkg/Dead")
	for range 3 {
		got, err := run()
		require.NoError(t, err, "%s", got)
		require.Equal(t, string(want), string(got))
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := run()
			if err != nil {
				t.Errorf("CLI: %v: %s", err, got)
				return
			}
			if string(got) != string(want) {
				t.Errorf("CLI differs: %s", got)
			}
		}()
	}
	wg.Wait()
}
