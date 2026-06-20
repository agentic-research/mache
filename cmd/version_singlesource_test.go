package cmd

import (
	"testing"

	"github.com/agentic-research/mache/internal/buildinfo"
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
