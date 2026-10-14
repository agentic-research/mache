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

// BinaryVersion is the exact ley-line-open release mache pins, downloads and
// verifies. Exported so artifact generators (tools/server-json-gen) can derive
// the dependency version mache publishes rather than restating it — server.json
// previously carried an independently-maintained "0.4.5" that had fallen four
// minors behind this pin.
const BinaryVersion = leylineBinaryVersion

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
//	  --jq '.assets[]|select(.name|test("^leyline-(darwin|linux)-(amd64|arm64)$"))|"\(.name) \(.digest)"'
var leylinePinnedSHA256 = map[string]string{
	"darwin-amd64": "7bd2bd8f82185ba01b1496f947afca54ab08932888848d4d65eb1de0d6e1bb41",
	"darwin-arm64": "a869e4f3d44539aed576928009dbf630cb9e0c4dc9fb4bb6fe9f948bd6e51765",
	"linux-amd64":  "48cc6dd242d25d1afe7fdde3418be26294a2d34def2e14e2b0b3932c8febebe2",
	"linux-arm64":  "0773dde8b0fbf36432f9ba04a53657de15c0c5b118ce24f93a55d13e641376de",
}

// leylineVersionMatchesPin runs `<path> --version` and reports whether the
// binary's version EXACTLY equals leylineBinaryVersion (major.minor.patch).
//
// The pin was originally major.minor with patch floating, on the theory that
// a patch bump is a daemon-only fix. LLO's release practice broke that
// assumption: v0.7.4 added node_refs.container_node_id and v0.7.5 added
// node_defs.canonical_kind — patch releases that CHANGED the emitted _ast
// schema. Under the floating pin, a PATH leyline 0.7.3 satisfied a v0.7.5 pin
// and silently produced dbs missing the columns the smell gate's rules probe
// for, zeroing fan_out_skew/untested_function locally while CI (which
// downloads the exact pinned release, SHA-verified) saw them — precisely the
// divergence the pin exists to prevent (mache-608a3c). A binary that won't
// run or whose version can't be parsed does NOT match: mache must refuse an
// unverifiable leyline rather than let a wrong-version parse silently diverge
// from CI. Only the exact pinned version is accepted.
func leylineVersionMatchesPin(path string) bool {
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		return false
	}
	got := ExtractSemver(string(out))
	if got == "" {
		return false
	}
	want := parseSemverParts(leylineBinaryVersion)
	have := parseSemverParts(got)
	return want == have
}

// ExtractSemver pulls the first "MAJOR.MINOR[.PATCH]" token from a version line
// such as "leyline 0.7.0 (open)". Returns "" when none is present.
//
// Exported because lltest must parse the same line when resolving an override
// binary, and a second implementation of "read leyline's version" is exactly
// the drift this repo keeps paying for elsewhere.
func ExtractSemver(s string) string {
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
