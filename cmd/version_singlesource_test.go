package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentic-research/mache/internal/buildcache"
	"github.com/agentic-research/mache/internal/buildinfo"
	"github.com/agentic-research/mache/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These guard the version single-sourcing (PR: version-single-source).
// Before it, MacheProducerVersion was the literal placeholder "0.x.y" and
// the binary version drifted from server.json. If anyone re-hardcodes a
// version here, one of these fails.

func TestVersionDerivesFromBuildinfo(t *testing.T) {
	if Version != buildinfo.Version {
		t.Errorf("cmd.Version %q != buildinfo.Version %q — version must be single-sourced", Version, buildinfo.Version)
	}
}

// TestProducerVersionInjectionHolds is the reworked producer-identity guard
// (B3, mache-96c378 stage 7): the identity is now injected — cmd/register.go
// calls buildcache.SetProducerVersion(Version) — so what must hold is the
// WIRING: whatever cmd.Version resolved to is exactly what buildcache stamps
// into lockfiles. An empty or placeholder identity means the injection broke.
func TestProducerVersionInjectionHolds(t *testing.T) {
	require.NotEmpty(t, Version)
	assert.Equal(t, Version, buildcache.ProducerVersion(),
		"cmd/register.go must have injected cmd.Version as the producer identity")
	assert.NotEqual(t, "0.x.y", buildcache.ProducerVersion())
}

// TestREADMEImageTagsMatchBuildinfo catches copy-pasteable version rot in
// the README's deployment instructions.
//
// This is not hypothetical: the README advertised `mache:0.11.0` and the
// Taskfile's apko invocation hardcoded `mache:0.8.0` while buildinfo said
// 0.18.0 — three different versions, so `task image` produced a
// ten-release-stale tag and the documented `docker run` line referenced an
// image that was never published. Nothing gated either one.
func TestREADMEImageTagsMatchBuildinfo(t *testing.T) {
	version := buildinfo.Version

	raw, err := os.ReadFile(filepath.Join(testutil.MacheRepoRoot(t), "README.md"))
	require.NoError(t, err, "read README.md")
	doc := string(raw)

	// Scan for `mache:<semver>` occurrences without a regex: take the run of
	// version characters following each "mache:" and require it to be the
	// current version. An optional leading `v` is accepted because the
	// published ghcr tags are v-prefixed (`ghcr.io/agentic-research/mache:v0.20.0`)
	// while the local apko tag is not (`mache:0.20.0`) — both are copy-pasteable
	// and both must track buildinfo. Non-version suffixes (e.g. `mache:latest`,
	// or prose that happens to contain "mache:") are ignored.
	const marker = "mache:"
	var stale []string
	for idx := 0; ; {
		hit := strings.Index(doc[idx:], marker)
		if hit < 0 {
			break
		}
		start := idx + hit + len(marker)
		end := start
		if end < len(doc) && doc[end] == 'v' {
			end++
		}
		for end < len(doc) && (doc[end] == '.' || (doc[end] >= '0' && doc[end] <= '9')) {
			end++
		}
		tag := doc[start:end]
		if tag != "" && tag != "v" && tag != version && tag != "v"+version {
			stale = append(stale, marker+tag)
		}
		idx = start
	}

	assert.Empty(t, stale,
		"README.md references image tags that disagree with "+
			"internal/buildinfo/version.txt (%s); these are copy-pasteable, so "+
			"a stale tag sends users to an image that does not exist", version)
}
