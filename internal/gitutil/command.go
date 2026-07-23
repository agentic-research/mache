// Package gitutil provides hermetic Git subprocess construction.
package gitutil

import (
	"os"
	"os/exec"
	"strings"
)

// HermeticGitCommand constructs a Git command without repository-local environment
// variables inherited from a parent Git hook. Those variables take precedence
// over cmd.Dir and `git -C`, which can make a nested command operate on the
// repository whose hook is running instead of its explicit target.
func HermeticGitCommand(args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Env = WithoutLocalEnv(os.Environ())
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
