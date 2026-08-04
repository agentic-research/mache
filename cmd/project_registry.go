package cmd

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zeebo/blake3"
)

// The local project-identity registry lets `mache init` bake a working
// per-project MCP URL without depending on the MCP roots protocol
// (mache-6ec106). The shared HTTP daemon routes every session by asking the
// connecting client for its workspace root over `roots/list` — a
// server-initiated request that requires the client to keep a channel open
// for server-pushed messages. Many real MCP HTTP clients are plain
// request/response and never open that channel, so for them root discovery
// isn't slow, it's structurally undeliverable.
//
// `mache init` (run once per project directory) registers that project's
// absolute path here under a salted, deterministic token, and embeds
// `?project=<token>` in the URL it writes to .claude/mcp.json. The daemon
// resolves that query param by looking the token up in THIS SAME local file
// — it never accepts or trusts a caller-supplied path directly. An attacker
// who guesses or independently computes a token gets a registry miss, not a
// disclosure, unless the exact path was already registered by a legitimate
// local `mache init` run. The salt is what makes a token infeasible to guess
// by hashing a candidate path: without it, a token would just be
// BLAKE3(path), which anyone could compute for any path they suspect exists.
//
// Tokens are deterministic (same salt + same path -> same token) rather than
// random, so re-running `mache init` in a directory reproduces the same
// token instead of orphaning the URL already written into every client's
// config — matching the hash-based identity mache already uses for content
// (leyline's node_hash / merkle IR) rather than inventing a separate
// random-UUID scheme.

const (
	projectSaltFile     = "project-salt"
	projectRegistryFile = "projects.json"
)

// macheHomeDir returns ~/.mache, creating it if necessary.
func macheHomeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	dir := filepath.Join(home, ".mache")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	return dir, nil
}

// loadOrCreateProjectSalt returns the per-machine salt used to derive
// project tokens, generating and persisting a new 32-byte random salt on
// first use.
func loadOrCreateProjectSalt() ([]byte, error) {
	dir, err := macheHomeDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, projectSaltFile)

	if data, err := os.ReadFile(path); err == nil {
		if len(data) == 32 {
			return data, nil
		}
		// Wrong length: a corrupt or foreign file. Fall through and
		// regenerate rather than trusting partial/garbage salt bytes.
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generate project salt: %w", err)
	}
	if err := writeFileAtomic(path, salt); err != nil {
		return nil, fmt.Errorf("write %s: %w", path, err)
	}
	return salt, nil
}

// projectToken derives a deterministic, salted token for absPath. Truncated
// to 128 bits (32 hex chars) — plenty against guessing given the salt, short
// enough to sit comfortably in a URL query parameter.
func projectToken(salt []byte, absPath string) string {
	h := blake3.New()
	_, _ = h.Write(salt)
	_, _ = h.Write([]byte(absPath))
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:16])
}

// projectRegistryPath is ~/.mache/projects.json, the token -> absolute-path
// map.
func projectRegistryPath() (string, error) {
	dir, err := macheHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, projectRegistryFile), nil
}

// loadProjectRegistry reads the token -> path map, returning an empty map
// (not an error) when the registry has not been created yet.
func loadProjectRegistry() (map[string]string, error) {
	path, err := projectRegistryPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	reg := map[string]string{}
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return reg, nil
}

// registerProject records absPath in the local registry under its derived
// token and returns the token. Idempotent: re-registering the same path
// (e.g. re-running `mache init`) reproduces the same token and simply
// overwrites its own entry — every other project's entry is preserved.
func registerProject(absPath string) (string, error) {
	salt, err := loadOrCreateProjectSalt()
	if err != nil {
		return "", err
	}
	token := projectToken(salt, absPath)

	reg, err := loadProjectRegistry()
	if err != nil {
		return "", err
	}
	reg[token] = absPath

	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal project registry: %w", err)
	}
	path, err := projectRegistryPath()
	if err != nil {
		return "", err
	}
	if err := writeFileAtomic(path, append(data, '\n')); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return token, nil
}

// resolveProjectToken looks up token in the local registry, returning the
// absolute path it was registered against. ok is false when the token is
// unknown — whether guessed, stale, or from a registry that was wiped after
// the client's config was written.
func resolveProjectToken(token string) (string, bool) {
	reg, err := loadProjectRegistry()
	if err != nil {
		return "", false
	}
	path, ok := reg[token]
	return path, ok
}
