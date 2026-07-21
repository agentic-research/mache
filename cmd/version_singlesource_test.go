package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentic-research/mache/internal/buildinfo"
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

func TestMacheProducerVersionIsRealNotPlaceholder(t *testing.T) {
	if MacheProducerVersion == "0.x.y" {
		t.Fatal("MacheProducerVersion is still the 0.x.y placeholder")
	}
	if MacheProducerVersion != buildinfo.Version {
		t.Errorf("MacheProducerVersion %q != buildinfo.Version %q", MacheProducerVersion, buildinfo.Version)
	}
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

	raw, err := os.ReadFile(filepath.Join(macheRepoRoot(t), "README.md"))
	require.NoError(t, err, "read README.md")
	doc := string(raw)

	// Scan for `mache:<semver>` occurrences without a regex: take the run of
	// version characters following each "mache:" and require it to be the
	// current version. Non-version suffixes (e.g. `mache:latest`, or prose
	// that happens to contain "mache:") are ignored.
	const marker = "mache:"
	var stale []string
	for idx := 0; ; {
		hit := strings.Index(doc[idx:], marker)
		if hit < 0 {
			break
		}
		start := idx + hit + len(marker)
		end := start
		for end < len(doc) && (doc[end] == '.' || (doc[end] >= '0' && doc[end] <= '9')) {
			end++
		}
		if tag := doc[start:end]; tag != "" && tag != version {
			stale = append(stale, marker+tag)
		}
		idx = start
	}

	assert.Empty(t, stale,
		"README.md references image tags that disagree with "+
			"internal/buildinfo/version.txt (%s); these are copy-pasteable, so "+
			"a stale tag sends users to an image that does not exist", version)
}
