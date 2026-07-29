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
//	  --jq '.assets[]|select(.name|startswith("leyline-"))|"\(.name) \(.digest)"'
var leylinePinnedSHA256 = map[string]string{
	"darwin-amd64": "9871ae9dd17b7c685ba12d3ad4d103e719d10834a189c73c94872ad8262289d8",
	"darwin-arm64": "2018d879056e95058bbb5862eaf696252929a8f88fdf728a1a092db65c1beba8",
	"linux-amd64":  "d9c900bf81b3ba60a083ce55e0fea4a65f477cda730dcd27a08ce5127a37d414",
	"linux-arm64":  "2d994da88d938a3ae3c23aa87d427fcbd4f350d6ca8b7d306545abfcc9907dda",
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
	got := extractSemver(string(out))
	if got == "" {
		return false
	}
	want := parseSemverParts(leylineBinaryVersion)
	have := parseSemverParts(got)
	return want == have
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
