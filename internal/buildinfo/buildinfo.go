// Package buildinfo holds the canonical CLEAN release version for mache —
// the committed, drift-checked base that server.json (tools/server-json-gen)
// and the Wolfi/melange package derive from. It lives in version.txt, embedded
// at compile time, because those artifacts need a stable, valid semver that is
// independent of git state (an apk version can't be "0.9.0-9-gabc", and the
// committed server.json is drift-gated in CI).
//
// This is NOT the string the binary reports. `mache version` shows cmd.Version,
// which is injected from `git describe --tags` at build time (see cmd.Version),
// so the binary tells the truth about the built code — a clean tag on a release,
// a git-distance string between releases. version.txt is only the FALLBACK for a
// bare `go build`/`go test` that injects no ldflags.
//
// Before buildinfo, the version was duplicated across at least four sites that
// drifted independently (cmd.Version "dev", a never-filled MacheProducerVersion
// placeholder, server-json-gen, melange.yaml). Consolidating the clean base
// here removed that drift.
//
// To cut a release: bump version.txt to the new version, run `task gen:version`
// (stamps melange.yaml + regenerates server.json) and `task version:check`, then
// tag `v<version>` — release.yml gates that the tag matches this base.
package buildinfo

import (
	_ "embed"
	"strings"
)

//go:embed version.txt
var rawVersion string

// Version is the canonical mache release version in semver form, no "v"
// prefix (e.g. "0.9.0"). Callers that need a tag-style string add the "v".
var Version = strings.TrimSpace(rawVersion)
