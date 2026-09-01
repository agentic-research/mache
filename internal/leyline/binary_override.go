package leyline

import (
	"fmt"
	"log"
	"os"
	"os/exec"
)

// BinaryOverrideEnv is the explicit developer escape hatch for pointing mache
// at a leyline build other than the pinned release — a local `cargo build`, a
// bisect, a patched daemon.
//
// It exists because the pin is deliberately strict: ResolveBinary accepts ONLY
// leylineBinaryVersion at every tier (PATH, ~/.mache/bin, download), since LLO
// ships _ast schema changes in patch releases and a mismatched producer
// silently diverges local runs from CI (mache-608a3c). That strictness is
// correct for normal use and hostile to LLO development, which previously left
// no option but editing the pin constant.
//
// Contract, deliberately narrow:
//   - It is an explicit opt-in. Nothing sets it implicitly, and it is never
//     consulted as a fallback after a pin miss — an unset override must not
//     change resolution at all.
//   - It logs on every use. A silently-honoured override would reintroduce the
//     exact class of failure the pin prevents: a wrong-version producer that
//     nobody notices until numbers diverge.
//   - It fails loudly if the path does not exist, rather than falling back to
//     the pinned resolution. Someone who sets this wants THIS binary; quietly
//     substituting another is worse than an error.
const BinaryOverrideEnv = "MACHE_LEYLINE_BINARY"

// OverrideBinary returns the developer-specified leyline path when
// MACHE_LEYLINE_BINARY is set, and ("", false, nil) when it is not.
//
// Exported so the gated test resolvers (internal/lltest) honour the SAME
// override production does. They previously called CachedPinnedBinary directly
// and so ignored it entirely — meaning `mache build` would run a candidate
// while the conformance and parity gates silently kept testing the pin
// (mache-cc1a70).
func OverrideBinary() (string, bool, error) {
	p := os.Getenv(BinaryOverrideEnv)
	if p == "" {
		return "", false, nil
	}
	st, err := os.Stat(p)
	if err != nil {
		return "", true, fmt.Errorf("%s=%s: %w (unset it to use the pinned %s)", BinaryOverrideEnv, p, err, leylineBinaryVersion)
	}
	if st.IsDir() {
		return "", true, fmt.Errorf("%s=%s is a directory, not a leyline binary", BinaryOverrideEnv, p)
	}
	// Report the version we are actually about to run so the divergence is in
	// the log next to whatever it produces, not inferred later from a bad number.
	log.Printf("%s=%s overrides the pinned %s (reports %q) — results may diverge from CI",
		BinaryOverrideEnv, p, leylineBinaryVersion, binaryVersionString(p))
	return p, true, nil
}

// binaryVersionString reports what `<path> --version` says, or "unknown" when
// the binary will not run or prints nothing parseable. Best-effort: this is for
// the log line, so an unreadable version must not fail the override.
func binaryVersionString(path string) string {
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		return "unknown"
	}
	if v := ExtractSemver(string(out)); v != "" {
		return v
	}
	return "unknown"
}
