package buildinfo

import (
	"regexp"
	"testing"
)

// semver (no "v" prefix) with optional pre-release/build metadata.
var semverRe = regexp.MustCompile(`^\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?$`)

func TestVersionIsCleanSemver(t *testing.T) {
	if Version == "" {
		t.Fatal("buildinfo.Version is empty — version.txt failed to embed")
	}
	if Version != rawVersion && Version+"\n" != rawVersion {
		// Version must be the trimmed form of the embedded file; a stray
		// space would silently corrupt every downstream stamp.
		if got := len(Version); got == len(rawVersion) {
			t.Fatalf("Version not trimmed: %q vs raw %q", Version, rawVersion)
		}
	}
	if Version[0] == 'v' {
		t.Errorf("Version must not carry a 'v' prefix (got %q); callers add 'v' for tags", Version)
	}
	if !semverRe.MatchString(Version) {
		t.Errorf("Version %q is not semver-shaped (X.Y.Z[-meta])", Version)
	}
}
