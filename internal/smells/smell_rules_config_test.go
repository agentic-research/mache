package smells

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/internal/projcfg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProjectConfig_ParsesSmellRulesDir pins that the new smellRulesDir
// field round-trips from .mache.json through loadProjectConfig.
func TestProjectConfig_ParsesSmellRulesDir(t *testing.T) {
	dir := t.TempDir()
	cfg := `{"sources": [{"path": ".", "schema": "go"}], "smellRulesDir": "./smell-rules"}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, projcfg.ConfigFileName), []byte(cfg), 0o644))

	got, err := projcfg.LoadProjectConfig(dir)
	require.NoError(t, err)
	assert.Equal(t, "./smell-rules", got.SmellRulesDir)
}

// TestProjectConfig_SmellRulesDirOmittedIsEmpty pins the omitempty
// zero-value: a config without the field leaves SmellRulesDir "".
func TestProjectConfig_SmellRulesDirOmittedIsEmpty(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, projcfg.ConfigFileName),
		[]byte(`{"sources": [{"path": "."}]}`), 0o644))

	got, err := projcfg.LoadProjectConfig(dir)
	require.NoError(t, err)
	assert.Equal(t, "", got.SmellRulesDir)
}

// TestResolveSmellRulesDir_FlagWins pins the top of the precedence
// chain: an explicit flag value beats env and config.
func TestResolveSmellRulesDir_FlagWins(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, projcfg.ConfigFileName),
		[]byte(`{"sources": [{"path": "."}], "smellRulesDir": "/from/config"}`), 0o644))
	t.Setenv(SmellRulesEnvVar, "/from/env")

	got := ResolveSmellRulesDir("/from/flag", dir)
	assert.Equal(t, "/from/flag", got)
}

// TestResolveSmellRulesDir_EnvBeatsConfig pins env > config when no flag.
func TestResolveSmellRulesDir_EnvBeatsConfig(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, projcfg.ConfigFileName),
		[]byte(`{"sources": [{"path": "."}], "smellRulesDir": "/from/config"}`), 0o644))
	t.Setenv(SmellRulesEnvVar, "/from/env")

	got := ResolveSmellRulesDir("", dir)
	assert.Equal(t, "/from/env", got)
}

// TestResolveSmellRulesDir_ConfigRelativeResolvesAgainstProjectDir pins
// that a relative smellRulesDir is joined onto the .mache.json's own
// directory (portability) — not the process CWD.
func TestResolveSmellRulesDir_ConfigRelativeResolvesAgainstProjectDir(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, projcfg.ConfigFileName),
		[]byte(`{"sources": [{"path": "."}], "smellRulesDir": "rules"}`), 0o644))
	t.Setenv(SmellRulesEnvVar, "") // ensure env doesn't shadow config

	got := ResolveSmellRulesDir("", dir)
	assert.Equal(t, filepath.Join(dir, "rules"), got)
}

// TestResolveSmellRulesDir_ConfigAbsoluteUsedVerbatim pins that an
// absolute config value is returned as-is.
func TestResolveSmellRulesDir_ConfigAbsoluteUsedVerbatim(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, projcfg.ConfigFileName),
		[]byte(`{"sources": [{"path": "."}], "smellRulesDir": "/abs/rules"}`), 0o644))
	t.Setenv(SmellRulesEnvVar, "")

	got := ResolveSmellRulesDir("", dir)
	assert.Equal(t, "/abs/rules", got)
}

// TestResolveSmellRulesDir_ConfigTraversalRejected pins the path-safety
// guard: a `../escape` value from an untrusted .mache.json is rejected
// (resolves to "") rather than escaping the project tree.
func TestResolveSmellRulesDir_ConfigTraversalRejected(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, projcfg.ConfigFileName),
		[]byte(`{"sources": [{"path": "."}], "smellRulesDir": "../escape"}`), 0o644))
	t.Setenv(SmellRulesEnvVar, "")

	got := ResolveSmellRulesDir("", dir)
	assert.Equal(t, "", got, "a traversal escape must be rejected, not resolved")
}

// TestResolveSmellRulesDir_NoneReturnsEmpty pins the bottom of the
// chain: no flag, no env, no config -> "".
func TestResolveSmellRulesDir_NoneReturnsEmpty(t *testing.T) {
	dir := t.TempDir() // no .mache.json here
	t.Setenv(SmellRulesEnvVar, "")

	got := ResolveSmellRulesDir("", dir)
	assert.Equal(t, "", got)
}

// TestResolveSmellRulesDir_EmptyConfigFieldFallsThrough pins that a
// .mache.json present but WITHOUT smellRulesDir yields "".
func TestResolveSmellRulesDir_EmptyConfigFieldFallsThrough(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, projcfg.ConfigFileName),
		[]byte(`{"sources": [{"path": "."}]}`), 0o644))
	t.Setenv(SmellRulesEnvVar, "")

	got := ResolveSmellRulesDir("", dir)
	assert.Equal(t, "", got)
}
