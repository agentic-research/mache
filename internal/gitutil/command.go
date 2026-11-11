// Package gitutil provides hermetic Git subprocess construction.
package gitutil

import (
	"os"
	"os/exec"
	"strings"
)

// HermeticGitCommand constructs a Git command insulated from the surrounding
// developer's Git setup, in two ways.
//
// It drops repository-local environment variables inherited from a parent Git
// hook. Those take precedence over cmd.Dir and `git -C`, which can make a
// nested command operate on the repository whose hook is running instead of
// its explicit target.
//
// It also disables HOOKS. A developer with a global `core.hooksPath` — rsry
// installs one that rejects any commit message without a bead reference — had
// their hooks run inside mache's own temp repositories, and seven tests in
// internal/mcpserve failed on `git commit -m "init"` for a machine-local
// reason CI cannot see. Hooks are wrong for every call site here regardless:
// these are read-only queries (rev-parse, log, remote get-url) and throwaway
// clones, none of them a commit the developer authored.
func HermeticGitCommand(args ...string) *exec.Cmd {
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=" + os.DevNull}, args...)...)
	cmd.Env = WithoutLocalEnv(os.Environ())
	return cmd
}

// IsolatedGitCommand is HermeticGitCommand for a read of repository HISTORY
// whose output must be a function of the repository alone: it also switches
// off the user's global config, the system config, and replace refs.
//
// Each of those changes what `git log` prints for the same commits. A global
// `i18n.logOutputEncoding` re-encodes every message; `log.*` and `pretty.*`
// keys reshape output; a `refs/replace/` entry substitutes another object's
// content under the original SHA. None of that is the repository's history,
// and mache ingests history as data (internal/ingest.LoadGitCommits), so two
// machines projecting the same repository must get the same records.
//
// It is deliberately NOT folded into HermeticGitCommand, because its other
// callers need the user's configuration. The `git clone` calls in
// internal/mcpserve fetch from URLs: `credential.helper`, `url.*.insteadOf`,
// `http.*` and `core.sshCommand` are how a private repository is reachable
// at all. cmd/agent's `rev-parse` and `remote get-url` run on the developer's
// own checkout, where `safe.directory` and `remote.*` are theirs to set.
//
// The one cost: a repository the user can read only through a global
// `safe.directory` entry is refused here with git's own "dubious ownership"
// error, instead of silently reading it under a config mache cannot see.
func IsolatedGitCommand(args ...string) *exec.Cmd {
	cmd := HermeticGitCommand(args...)
	// Later entries win in exec.Cmd.Env, so these override whatever the
	// developer's shell set. GIT_NO_REPLACE_OBJECTS is one of the local
	// variables WithoutLocalEnv strips (a parent hook's value must not leak
	// in), so it is set afresh here rather than inherited.
	cmd.Env = append(cmd.Env,
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_NO_REPLACE_OBJECTS=1",
	)
	return cmd
}

// WithoutLocalEnv removes the variables listed by
// `git rev-parse --local-env-vars`.
func WithoutLocalEnv(env []string) []string {
	clean := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if localEnv[name] {
			continue
		}
		clean = append(clean, entry)
	}
	return clean
}

var localEnv = map[string]bool{
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_COMMON_DIR":                   true,
	"GIT_CONFIG":                       true,
	"GIT_CONFIG_COUNT":                 true,
	"GIT_CONFIG_PARAMETERS":            true,
	"GIT_DIR":                          true,
	"GIT_GRAFT_FILE":                   true,
	"GIT_IMPLICIT_WORK_TREE":           true,
	"GIT_INDEX_FILE":                   true,
	"GIT_NO_REPLACE_OBJECTS":           true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_PREFIX":                       true,
	"GIT_REPLACE_REF_BASE":             true,
	"GIT_SHALLOW_FILE":                 true,
	"GIT_WORK_TREE":                    true,
}
