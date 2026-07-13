package leyline

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// leylinePinnedSHA256 pins the SHA-256 of each ley-line-open release asset for
// leylineBinaryVersion, keyed "<GOOS>-<GOARCH>". mache verifies every
// DOWNLOADED leyline against this: the binary it runs must be byte-identical to
// the published pinned release, the same supply-chain posture the repo already
// enforces for GitHub Actions (task actions:lint). It is intentionally NOT
// applied to a PATH/cached binary — codesigning (macOS) mutates the Mach-O, so a
// legitimately-installed local copy of the pinned version has a different whole-
// file hash; those candidates are gated by version instead (see
// leylineVersionMatchesPin).
//
// Regenerate on a leylineBinaryVersion bump:
//
//	gh release view <tag> --repo agentic-research/ley-line-open --json assets \
//	  --jq '.assets[]|select(.name|startswith("leyline-"))|"\(.name) \(.digest)"'
var leylinePinnedSHA256 = map[string]string{
	"darwin-amd64": "63ba1c9e3559ecb8d492e3e6ed3fabce98a634c6048943c9ef9a45b49432a2a0",
	"darwin-arm64": "7e6182b16324aa33e43c039d274440b8e9a2c84b8ba8a2a5dce385123003fae1",
	"linux-amd64":  "3a75973d9ef89a3f1a89ef2d29fdc3d2f0da8345d572981812830ac9bf26bab4",
	"linux-arm64":  "191e0ee872e095791f8d2f22c019832bcf73b28e6921c960b858d0b97433e69f",
}

// leylineVersionMatchesPin runs `<path> --version` and reports whether the
// binary's major.minor equals leylineBinaryVersion's. Patch floats — per the
// pin contract, major.minor is the wire/schema surface, and a patch bump is a
// daemon-only fix (see the leylineBinaryVersion doc). A binary that won't run or
// whose version can't be parsed does NOT match: mache must refuse an
// unverifiable leyline rather than let a wrong-version parse silently diverge
// from CI. This is the guard against "never use the local version / a raw-main
// build" — only the pinned version is accepted.
func leylineVersionMatchesPin(path string) bool {
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		return false
	}
	got := extractSemver(string(out))
	if got == "" {
		return false
	}
	want := parseSemverParts(leylineBinaryVersion)
	have := parseSemverParts(got)
	return want[0] == have[0] && want[1] == have[1]
}

// extractSemver pulls the first "MAJOR.MINOR[.PATCH]" token from a version line
// such as "leyline 0.7.0 (open)". Returns "" when none is present.
func extractSemver(s string) string {
	for tok := range strings.FieldsSeq(s) {
		t := strings.TrimPrefix(tok, "v")
		i := strings.IndexByte(t, '.')
		if i <= 0 {
			continue
		}
		allDigits := true
		for _, r := range t[:i] {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			return t
		}
	}
	return ""
}

// verifyLeylineSHA256 checks that the file at path hashes to the pinned SHA-256
// for the current platform. An unknown platform (no pin) is an error — refuse to
// trust a binary we can't verify rather than proceed.
func verifyLeylineSHA256(path string) error {
	key := runtime.GOOS + "-" + runtime.GOARCH
	want, ok := leylinePinnedSHA256[key]
	if !ok {
		return fmt.Errorf("no pinned leyline SHA-256 for platform %s (leyline %s)", key, leylineBinaryVersion)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("leyline download SHA-256 mismatch for %s: got %s, pinned %s (leyline %s) — refusing a binary that is not the published pinned release",
			key, got, want, leylineBinaryVersion)
	}
	return nil
}
