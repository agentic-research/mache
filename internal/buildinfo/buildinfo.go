// Package buildinfo is the single source of truth for mache's release
// version. The version lives in version.txt (embedded at compile time) so
// that every consumer — the binary's `mache version` output, the build-cache
// lockfile producer field, the generated server.json, and the Wolfi/melange
// package — derives the same value from one place.
//
// Before buildinfo, the version was duplicated across at least four sites that
// drifted independently:
//
//   - cmd.Version            "dev" (ldflags from git tag; goreleaser never
//     injected it, so release binaries reported "dev")
//   - cmd.MacheProducerVersion "0.x.y" (a never-filled placeholder)
//   - server-json-gen.serverVersion "0.8.0"
//   - melange.yaml version:  0.8.0
//
// To bump the version, edit version.txt and run `task version:check` to
// confirm the git tag and melange.yaml agree.
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
