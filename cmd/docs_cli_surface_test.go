package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/agentic-research/mache/internal/testutil"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
	"mvdan.cc/sh/v3/syntax"
)

// Doc-vs-CLI drift gate.
//
// # Why this exists
//
// Three separate documented invocations were found broken in a single day:
//
//  1. examples/README.md documented `mache mount <source> --schema <s> <mnt>`.
//     There is no `mount` subcommand — mounting is the ROOT command, and the
//     documented three-positional shape errors with "accepts at most 1 arg(s),
//     received 3" (mache-d55457).
//  2. The published image bakes `serve` into its ENTRYPOINT, so an invocation
//     written against a different image expands to `mache serve serve …`
//     (mache-504adc).
//  3. `mache build --schema … <file.json> out.db` — the documented data-source
//     flow — exits 1 because build unconditionally routes through
//     `leyline parse`, which requires a directory (mache-d55457, half two).
//
// The repo already pins docs to ground truth twice, and both work:
// TestREADMEToolMatrixMatchesRegistry (README matrix vs the live MCP registry)
// and TestREADMEImageTagsMatchBuildinfo (README image tags vs buildinfo). This
// is the same shape aimed at invocation lines.
//
// # What this gate PROVES
//
// For every `mache …` invocation in a shell code block of README.md,
// GETTING-STARTED.md, examples/README.md and docs/*.md:
//
//   - the named subcommand exists in the live cobra tree (rootCmd.Find), or the
//     line is the root-command form;
//   - every flag exists on the command it is attached to (pflag lookup against
//     that command's own FlagSet, including inherited persistent flags);
//   - the positional count satisfies that command's cobra.Args validator;
//   - for a documented `docker run`, the EFFECTIVE argv — the image's real
//     ENTRYPOINT plus the documented args, which replace CMD — is itself a
//     valid mache invocation.
//
// Ground truth is the cobra command tree, pflag's flag registry, and the
// in-repo image definitions — the structures the real binary and the real image
// build dispatch through. No hardcoded subcommand list, no scrape of `--help`
// text. Commands are recovered from a real POSIX-shell parse (mvdan.cc/sh),
// following the precedent in internal/lint/tool_preflight.go, so quoting,
// pipelines, comments, line continuations and `$(…)` never masquerade as argv.
//
// # What this gate does NOT prove
//
// It does not prove a documented command SUCCEEDS. It proves the command gets
// past cobra's argument parsing and reaches RunE. Instance 3 above is exactly
// that gap: `mache build --schema … x.json out.db` is a well-formed invocation
// (build takes ExactArgs(2)) that then fails at runtime inside leyline. This
// gate passes it and will keep passing it. Catching that class needs execution,
// which is out of scope: the documented commands variously need a network, an
// NFS mount, a long-lived daemon, or a real source tree.
//
// It does not run a container. ENTRYPOINT/CMD are read from the in-repo image
// definitions that a release builds from, not from a pulled image.
//
// Prose outside code fences is checked only for subcommand existence (see
// checkBareWordSubcommand) — prose writes fragments like `mache cache`, so
// arity and flag completeness are not meaningful there.
//
// # Why a Go test rather than a smell rule or a shell script
//
// The ground truth is a Go data structure that only exists in-process. A
// find_smells SQL rule reads the projected graph; it cannot enumerate
// rootCmd.Commands() or ask pflag whether `--stdio` takes a value. A shell
// script could only scrape `--help`, which is the textual matching this repo
// treats as a smell. The precedent tests are Go tests in package cmd, so the
// Taskfile target is the existing `task test` — no new target, and local/CI
// stay 1:1 by construction.
//
// Executing `mache <sub> --help` per documented subcommand was considered and
// deliberately rejected: cobra returns flag.ErrHelp right after ParseFlags and
// BEFORE ValidateArgs, so `--help` would have exited 0 on instance 1's
// three-positional line. It proves strictly less than Find + ValidateArgs, at
// the cost of a subprocess and a built binary.

// docCall is one command recovered from a documentation file, carried with its
// source location so a failure names the exact line to edit.
type docCall struct {
	File   string
	Line   int
	Argv   []string
	Fenced bool // true inside a shell fence, false for an inline `code` span
}

func (d docCall) where() string {
	return fmt.Sprintf("%s:%d: %s", d.File, d.Line, strings.Join(d.Argv, " "))
}

// gatedDocs is the set of documentation files whose invocations must match the
// CLI. docs/adr/** is deliberately excluded: ADRs are a dated historical record
// and may legitimately quote a CLI surface that has since changed.
func gatedDocs(t *testing.T) []string {
	t.Helper()
	root := testutil.MacheRepoRoot(t)
	files := []string{
		filepath.Join(root, "README.md"),
		filepath.Join(root, "GETTING-STARTED.md"),
		filepath.Join(root, "examples", "README.md"),
	}
	globbed, err := filepath.Glob(filepath.Join(root, "docs", "*.md"))
	require.NoError(t, err, "glob docs/*.md")
	sort.Strings(globbed)
	return append(files, globbed...)
}

// docCallsInGatedDocs returns every command recovered from the gated docs.
func docCallsInGatedDocs(t *testing.T) []docCall {
	t.Helper()
	root := testutil.MacheRepoRoot(t)
	var all []docCall
	for _, path := range gatedDocs(t) {
		raw, err := os.ReadFile(path)
		require.NoError(t, err, "read %s", path)
		rel, err := filepath.Rel(root, path)
		require.NoError(t, err)
		all = append(all, documentedCalls(rel, string(raw))...)
	}
	return all
}

func TestDocumentedCommandsMatchCLISurface(t *testing.T) {
	var checked int
	for _, call := range docCallsInGatedDocs(t) {
		for _, argv := range macheArgvs(call.Argv) {
			checked++
			problem := checkBareWordSubcommand(argv)
			if call.Fenced && problem == "" {
				problem = validateInvocation(argv)
			}
			assert.Empty(t, problem,
				"%s\n  → %s\n  This documented invocation does not match the live "+
					"cobra command tree. Fix the doc line, or add the command/flag "+
					"it claims.", call.where(), problem)
		}
	}

	// A gate that silently stops finding anything is worse than no gate: if the
	// extractor breaks (fence syntax change, doc reshuffle) this fails loudly
	// rather than passing vacuously. The floor sits well under the real count.
	assert.Greater(t, checked, 20,
		"extracted only %d documented mache invocations across %d files — the "+
			"extractor has probably stopped matching, not the docs stopped "+
			"documenting", checked, len(gatedDocs(t)))
}

// TestDocumentedDockerRunMatchesImageEntrypoint checks the other half of
// mache-504adc: `docker run IMAGE <args>` APPENDS args to the image's
// ENTRYPOINT and REPLACES its CMD, so the command a reader actually runs is
// ENTRYPOINT ++ args. mache ships two image definitions with different
// entrypoints, and a line written for one is wrong for the other:
//
//	apko.yaml           → ENTRYPOINT [mache]         CMD [serve --stdio]
//	Dockerfile.release  → ENTRYPOINT [mache serve]   CMD [/source]
//
// Both shapes were confirmed against real images (`docker inspect` on
// ghcr.io/agentic-research/mache, and an apko build of a minimal config to
// verify apko emits exec-form ENTRYPOINT rather than a /bin/sh -c wrapper).
func TestDocumentedDockerRunMatchesImageEntrypoint(t *testing.T) {
	images := imageDefinitions(t)

	var checked int
	for _, call := range docCallsInGatedDocs(t) {
		if !call.Fenced {
			continue
		}
		ref, args, ok := dockerRunTarget(call.Argv)
		if !ok {
			continue
		}
		def, known := images.forRef(ref)
		require.True(t, known,
			"%s\n  references image %q, which no in-repo image definition builds. "+
				"Add it to imageDefinitions so its entrypoint stays gated.",
			call.where(), ref)

		checked++
		effective := append(append([]string{}, def.Entrypoint...), args...)
		if len(args) == 0 {
			effective = append(append([]string{}, def.Entrypoint...), def.Cmd...)
		}
		problem := validateInvocation(effective)
		assert.Empty(t, problem,
			"%s\n  image %s (defined in %s) has ENTRYPOINT %v, and `docker run` args "+
				"REPLACE CMD while APPENDING to ENTRYPOINT — so this line really runs:\n"+
				"    %s\n  → %s",
			call.where(), ref, def.Source, def.Entrypoint, strings.Join(effective, " "), problem)
	}

	assert.Positive(t, checked,
		"no documented `docker run` against a mache image was found — the README "+
			"documents at least one, so the extractor has regressed")
}

// TestDocsCLIGateDetectsDrift proves the gate can fail.
//
// A gate that cannot go red is worse than no gate — this repo has a
// placeholder smell rule that returns zero rows and therefore caught none of
// the drift above. So every drift class this gate claims to catch is exercised
// here against synthetic documentation, and every legitimate invocation shape
// found in the real docs is exercised as a negative control. If someone
// loosens the checker, these fail before the real docs do.
func TestDocsCLIGateDetectsDrift(t *testing.T) {
	for _, tc := range docsCLIDriftCases() {
		t.Run(tc.name, func(t *testing.T) {
			var complaints []string
			for _, call := range documentedCalls("synthetic.md", tc.doc) {
				for _, argv := range macheArgvs(call.Argv) {
					problem := checkBareWordSubcommand(argv)
					if call.Fenced && problem == "" {
						problem = validateInvocation(argv)
					}
					if problem != "" {
						complaints = append(complaints, problem)
					}
				}
			}
			if tc.wantHit == "" {
				assert.Empty(t, complaints,
					"this shape appears in the real docs and must not be flagged")
				return
			}
			assert.Contains(t, strings.Join(complaints, "; "), tc.wantHit,
				"the gate must reject this documented invocation")
		})
	}
}

// driftCase is one synthetic documentation snippet and the complaint it must
// (or must not) produce.
type driftCase struct {
	name    string
	doc     string
	wantHit string // substring of the expected complaint; "" means must pass
}

func docsCLIDriftCases() []driftCase {
	return []driftCase{
		// --- drift that MUST be caught ---
		{
			name:    "nonexistent subcommand (mache-d55457)",
			doc:     "```bash\nmache mount examples/x.json --schema s.json /tmp/audit\n```",
			wantHit: `"mount" is not a mache subcommand`,
		},
		{
			name:    "nonexistent subcommand with a plausible path arg",
			doc:     "```bash\nmache mount /tmp/audit\n```",
			wantHit: `"mount" is not a mache subcommand`,
		},
		{
			name:    "nonexistent subcommand in prose",
			doc:     "Run `mache mount` to project it.",
			wantHit: `"mount" is not a mache subcommand`,
		},
		{
			name:    "flag that does not exist on the subcommand",
			doc:     "```bash\nmache serve --not-a-real-flag ./src\n```",
			wantHit: `"mache serve" has no flag --not-a-real-flag`,
		},
		{
			name:    "shorthand that does not exist on the subcommand",
			doc:     "```bash\nmache serve --schema s.json -d ./src\n```",
			wantHit: `"mache serve" has no flag -d`,
		},
		{
			name:    "too many positionals for the root command",
			doc:     "```bash\nmache --schema s.json /tmp/a /tmp/b\n```",
			wantHit: "accepts at most 1 arg",
		},
		{
			name:    "too few positionals for a subcommand",
			doc:     "```bash\nmache build ./src\n```",
			wantHit: "accepts 2 arg",
		},
		{
			name:    "drift nested behind an MCP client's -- handoff",
			doc:     "```bash\nclaude mcp add mache -- mache mount ./src\n```",
			wantHit: `"mount" is not a mache subcommand`,
		},
		{
			name:    "drift inside a $(…) substitution",
			doc:     "```bash\n$(mache leyline nosuchthing) cdc enable\n```",
			wantHit: `"mache leyline" has no subcommand "nosuchthing"`,
		},

		// --- legitimate shapes that MUST keep passing ---
		{name: "root mount form", doc: "```bash\nmache --schema s.json --data d.json /tmp/m\n```"},
		{name: "root form with clustered bool shorthands", doc: "```bash\nmache -qw /tmp/m\n```"},
		{name: "serve with an explicit path and positional snapshot", doc: "```bash\nmache serve --stdio --path . ./code.db\n```"},
		{name: "repeatable flag", doc: "```bash\nmache serve --mount a=./a --mount b=./b\n```"},
		{name: "nested subcommand", doc: "```bash\nmache cache push --db ./m.db ./out\n```"},
		{name: "proxy subcommand forwards foreign flags", doc: "```bash\nmache leyline exec -- cdc enable --db x.db\n```"},
		{name: "proxy subcommand without the terminator", doc: "```bash\nmache leyline exec cdc gc --db x.db\n```"},
		{name: "substitution composing a downstream tool", doc: "```bash\n$(mache leyline path) cdc enable --db x.db\n```"},
		{name: "handoff to a valid nested command", doc: "```bash\nclaude mcp add mache -- mache serve --stdio --path .\n```"},
		{name: "trailing comment and backgrounding", doc: "```bash\nmache serve . &   # keep this running\n```"},
		{name: "line continuation", doc: "```bash\nmache cache verify \\\n    --remote https://x --scope s --ref latest\n```"},
		{name: "another tool that merely names mache", doc: "```bash\ncodex mcp add mache --url http://localhost:7532/mcp\n```"},
		{name: "prose fragment", doc: "See `mache cache` and `mache unmount <mountpoint>`."},
		{name: "untagged ASCII diagram is not shell", doc: "```\nmache (Engine + ASTWalker) → MemoryStore → MCP\n```"},
		{name: "json block is not shell", doc: "```json\n{\"cmd\": \"mache mount\"}\n```"},
	}
}

// TestDockerEntrypointGateDetectsDrift proves the docker half goes red on the
// exact mache-504adc shape: repeating `serve` after an image whose ENTRYPOINT
// already ends in `serve`. Verified against the real published image, which
// answers `Error: accepts at most 1 arg(s), received 2`.
func TestDockerEntrypointGateDetectsDrift(t *testing.T) {
	release, ok := imageDefinitions(t).forRef("ghcr.io/agentic-research/mache:v0.20.0")
	require.True(t, ok, "the published image must have an in-repo definition")
	require.Equal(t, []string{"mache", "serve"}, release.Entrypoint,
		"Dockerfile.release bakes `serve` into ENTRYPOINT; if that changes, "+
			"the README's docker line and this expectation both need revisiting")

	drifted := append(append([]string{}, release.Entrypoint...), "serve", "--stdio", "/source")
	assert.Contains(t, validateInvocation(drifted), "accepts at most 1 arg",
		"the gate must reject `docker run IMAGE serve …` against an ENTRYPOINT "+
			"that already ends in `serve`")

	correct := append(append([]string{}, release.Entrypoint...), "--stdio", "/source")
	assert.Empty(t, validateInvocation(correct),
		"the corrected form must pass — it was verified against the real image")

	// The local apko image has a different entrypoint, so the SAME doc line is
	// right there and wrong above. Modelling one image for both is the bug.
	local, ok := imageDefinitions(t).forRef("mache:0.20.0")
	require.True(t, ok)
	require.Equal(t, []string{"mache"}, local.Entrypoint)
	assert.Empty(t, validateInvocation(
		append(append([]string{}, local.Entrypoint...), "serve", "--stdio", "/src")))
}

// ---------------------------------------------------------------------------
// Validation against the live cobra tree
// ---------------------------------------------------------------------------

// validateInvocation runs argv through the same resolution the binary does:
// cobra resolves the subcommand, pflag says which tokens are flags and which
// consume a value, and the resolved command's own Args validator judges the
// positionals. Returns "" when the invocation is well-formed.
func validateInvocation(argv []string) string {
	target, remaining, err := rootCmd.Find(argv[1:])
	if err != nil {
		return err.Error()
	}
	// A proxy subcommand forwards its argv verbatim (leyline exec), so the
	// flags after it belong to the downstream tool, not to mache. cobra records
	// that intent structurally; honour it instead of reporting foreign flags.
	if target.DisableFlagParsing {
		return ""
	}

	positionals, unknown := splitFlagsAndArgs(target, remaining)
	if len(unknown) > 0 {
		return fmt.Sprintf("%q has no flag %s", target.CommandPath(), strings.Join(unknown, ", "))
	}
	// A group command (`cache`, `leyline`) is not runnable itself, so a
	// leftover positional is a subcommand claim, not an argument. cobra does
	// not reject it — Find falls back to the group, ArbitraryArgs accepts
	// anything, and the binary prints help and exits 0 — but in documentation
	// it is exactly the instance-1 defect one level down.
	if !target.Runnable() && target.HasSubCommands() && len(positionals) > 0 {
		return fmt.Sprintf("%q has no subcommand %q (have: %s)",
			target.CommandPath(), positionals[0], strings.Join(childNames(target), ", "))
	}
	if err := target.ValidateArgs(positionals); err != nil {
		return fmt.Sprintf("%q rejects %d positional arg(s) %v: %v",
			target.CommandPath(), len(positionals), positionals, err)
	}
	if !target.Runnable() && !target.HasSubCommands() {
		return fmt.Sprintf("%q is not runnable", target.CommandPath())
	}
	return ""
}

// checkBareWordSubcommand catches the instance-1 shape: a documented
// subcommand that does not exist.
//
// Cobra alone cannot catch it. `mache mount /tmp/x` is indistinguishable from
// the root form with a mountpoint positional, and `mache mount` on its own is a
// legal one-positional root invocation. The distinguishing fact is documentary,
// not structural: docs write the root command's mountpoint as a path
// (`/tmp/audit`, `./out`, `~/proj`) or a placeholder (`<mountpoint>`, `[dir]`),
// never as a bare lowercase word. So a bare-word first positional is a
// subcommand claim, and it must name a real child of rootCmd.
//
// This is the one heuristic in this file, and it is labelled as such.
func checkBareWordSubcommand(argv []string) string {
	positionals, _ := splitFlagsAndArgs(rootCmd, argv[1:])
	if len(positionals) == 0 {
		return ""
	}
	first := positionals[0]
	if !isBareWord(first) {
		return ""
	}
	for _, sub := range rootCmd.Commands() {
		if sub.Name() == first {
			return ""
		}
		for _, alias := range sub.Aliases {
			if alias == first {
				return ""
			}
		}
	}
	return fmt.Sprintf("%q is not a mache subcommand (have: %s)",
		first, strings.Join(subcommandNames(), ", "))
}

func subcommandNames() []string { return childNames(rootCmd) }

func childNames(cmd *cobra.Command) []string {
	names := make([]string, 0, len(cmd.Commands()))
	for _, sub := range cmd.Commands() {
		names = append(names, sub.Name())
	}
	sort.Strings(names)
	return names
}

// isBareWord reports whether s is shaped like a subcommand name: lowercase
// letters, digits and internal dashes only. Paths, placeholders, flags, URLs
// and NAME=VALUE pairs are all excluded by construction.
func isBareWord(s string) bool {
	if len(s) < 2 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0 && i < len(s)-1:
		default:
			return false
		}
	}
	return true
}

// splitFlagsAndArgs partitions args into positionals and unknown flags for cmd,
// asking pflag which flags exist and which consume a value. pflag marks
// value-less flags with a non-empty NoOptDefVal (bools set it to "true"), so
// "does this flag eat the next token" is answered by the registry rather than
// by a hardcoded list of boolean flags.
//
// Nothing here mutates cmd: flags are looked up, never parsed, so this cannot
// leak state into the other tests in package cmd that drive these commands.
func splitFlagsAndArgs(cmd *cobra.Command, args []string) (positionals, unknown []string) {
	flags := cmd.Flags()
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			// Everything after the terminator is positional.
			return append(positionals, args[i+1:]...), unknown

		case strings.HasPrefix(arg, "--"):
			name := strings.TrimPrefix(arg, "--")
			inline := false
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				name, inline = name[:eq], true
			}
			flag := flags.Lookup(name)
			if flag == nil {
				unknown = append(unknown, "--"+name)
				continue
			}
			if !inline && flag.NoOptDefVal == "" {
				i++ // consumes the following token as its value
			}

		case len(arg) > 1 && arg[0] == '-':
			// Shorthand, possibly clustered (-qw) or value-bearing (-sfile).
			shorthands := arg[1:]
			for j := 0; j < len(shorthands); j++ {
				flag := flags.ShorthandLookup(string(shorthands[j]))
				if flag == nil {
					unknown = append(unknown, "-"+string(shorthands[j]))
					continue
				}
				if flag.NoOptDefVal == "" {
					if j == len(shorthands)-1 {
						i++ // the value is the next token
					}
					break // the rest of this token is the value
				}
			}

		default:
			positionals = append(positionals, arg)
		}
	}
	return positionals, unknown
}

// ---------------------------------------------------------------------------
// Image definitions: ENTRYPOINT / CMD ground truth
// ---------------------------------------------------------------------------

// imageDefinition is one in-repo definition of a mache container image.
type imageDefinition struct {
	Source     string   // the file that defines it
	RefPrefix  string   // documented image refs that this definition builds
	Entrypoint []string // argv[0] normalised to "mache"
	Cmd        []string
}

type imageDefinitions_ []imageDefinition

func (defs imageDefinitions_) forRef(ref string) (imageDefinition, bool) {
	for _, def := range defs {
		if strings.HasPrefix(ref, def.RefPrefix) {
			return def, true
		}
	}
	return imageDefinition{}, false
}

// imageDefinitions reads ENTRYPOINT/CMD out of the two image definitions mache
// ships, rather than restating them.
func imageDefinitions(t *testing.T) imageDefinitions_ {
	t.Helper()
	root := testutil.MacheRepoRoot(t)
	apkoEntry, apkoCmd := parseApkoEntrypoint(t, filepath.Join(root, "apko.yaml"))
	dockerEntry, dockerCmd := parseDockerfileEntrypoint(t, filepath.Join(root, "Dockerfile.release"))
	return imageDefinitions_{
		// The published multi-arch release image.
		{
			Source:     "Dockerfile.release",
			RefPrefix:  "ghcr.io/agentic-research/mache",
			Entrypoint: dockerEntry,
			Cmd:        dockerCmd,
		},
		// The local distroless image `task image` produces and loads as
		// `mache:<version>`.
		{
			Source:     "apko.yaml",
			RefPrefix:  "mache:",
			Entrypoint: apkoEntry,
			Cmd:        apkoCmd,
		},
	}
}

// parseDockerfileEntrypoint reads the exec-form ENTRYPOINT/CMD from a
// Dockerfile. Only the exec (JSON array) form is supported, which is the form
// mache uses and the only one with unambiguous argv semantics.
func parseDockerfileEntrypoint(t *testing.T, path string) (entrypoint, cmd []string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "read %s", path)

	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		for _, directive := range []string{"ENTRYPOINT", "CMD"} {
			rest, ok := strings.CutPrefix(line, directive+" ")
			if !ok {
				continue
			}
			var argv []string
			require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(rest)), &argv),
				"%s: %s must use the exec (JSON array) form", path, directive)
			if directive == "ENTRYPOINT" {
				entrypoint = normaliseMacheArgv(argv)
			} else {
				cmd = argv
			}
		}
	}
	require.NotEmpty(t, entrypoint, "%s declares no ENTRYPOINT", path)
	return entrypoint, cmd
}

// parseApkoEntrypoint reads apko's entrypoint/cmd. apko emits an exec-form
// ENTRYPOINT from `entrypoint.command` (verified against a real apko build —
// it does NOT wrap in `/bin/sh -c`, which would swallow appended args) and
// shell-splits `cmd` into CMD.
func parseApkoEntrypoint(t *testing.T, path string) (entrypoint, cmd []string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "read %s", path)

	var config struct {
		Entrypoint struct {
			Command string `yaml:"command"`
		} `yaml:"entrypoint"`
		Cmd string `yaml:"cmd"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &config), "parse %s", path)
	require.NotEmpty(t, config.Entrypoint.Command, "%s declares no entrypoint.command", path)

	return normaliseMacheArgv(strings.Fields(config.Entrypoint.Command)), strings.Fields(config.Cmd)
}

// normaliseMacheArgv rewrites an absolute mache path to the bare binary name so
// the argv can be handed to validateInvocation.
func normaliseMacheArgv(argv []string) []string {
	out := append([]string{}, argv...)
	if len(out) > 0 && isMacheBinary(out[0]) {
		out[0] = "mache"
	}
	return out
}

// dockerRunTarget reports the image ref and post-image args of a `docker run`
// invocation. The image is the first argument that names a mache image; every
// token after it is a container arg. Finding the image this way avoids
// modelling docker's own flag table, which is not what this gate is about.
func dockerRunTarget(argv []string) (ref string, args []string, ok bool) {
	if len(argv) < 3 || argv[0] != "docker" || argv[1] != "run" {
		return "", nil, false
	}
	for i, tok := range argv[2:] {
		if tok == "--entrypoint" {
			// The documented line overrides the entrypoint, so the image's own
			// is not what runs. `task image:verify` covers that path.
			return "", nil, false
		}
		if isMacheImageRef(tok) {
			return tok, argv[2+i+1:], true
		}
	}
	return "", nil, false
}

// isMacheImageRef reports whether tok names a mache container image.
func isMacheImageRef(tok string) bool {
	name, _, tagged := strings.Cut(tok, ":")
	if !tagged || strings.HasPrefix(tok, "-") {
		return false
	}
	return name == "mache" || strings.HasSuffix(name, "/mache")
}

// ---------------------------------------------------------------------------
// Extraction: markdown → documented commands
// ---------------------------------------------------------------------------

// documentedCalls recovers every command in a markdown document: from shell
// fences (checked in full) and from inline code spans (subcommand existence
// only — prose writes fragments, so arity and flags are not meaningful).
func documentedCalls(file, doc string) []docCall {
	var out []docCall
	lines := strings.Split(doc, "\n")
	inFence, shellFence, fenceStart := false, false, 0
	var fence strings.Builder

	flushFence := func() {
		for _, call := range shellCalls(fence.String()) {
			out = append(out, docCall{
				File: file, Line: fenceStart + call.Line, Argv: call.Argv, Fenced: true,
			})
		}
		fence.Reset()
	}

	for i, line := range lines {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "```") {
			if inFence {
				if shellFence {
					flushFence()
				}
				inFence, shellFence = false, false
			} else {
				inFence = true
				// The markdown info string is the author's own declaration of
				// what the block is; trust it rather than guessing. A json,
				// mermaid, toml or untagged-diagram block carries no commands.
				shellFence = isShellFence(strings.TrimSpace(trimmed[3:]))
				fenceStart = i + 1
			}
			continue
		}
		if inFence {
			if shellFence {
				fence.WriteString(line)
				fence.WriteByte('\n')
			}
			continue
		}
		for _, span := range inlineCodeSpans(line) {
			for _, call := range shellCalls(span) {
				out = append(out, docCall{File: file, Line: i + 1, Argv: call.Argv})
			}
		}
	}
	return out
}

// isShellFence reports whether a fenced block's info string declares it to be
// shell commands.
func isShellFence(info string) bool {
	switch info {
	case "bash", "sh", "shell", "zsh", "console":
		return true
	}
	return false
}

// inlineCodeSpans returns the contents of every single-backtick span on a line.
func inlineCodeSpans(line string) []string {
	var spans []string
	for {
		open := strings.IndexByte(line, '`')
		if open < 0 {
			return spans
		}
		rest := line[open+1:]
		closed := strings.IndexByte(rest, '`')
		if closed < 0 {
			return spans
		}
		spans = append(spans, rest[:closed])
		line = rest[closed+1:]
	}
}

// shellCall is one command recovered from a shell parse, with its 1-based line
// within the parsed source.
type shellCall struct {
	Line int
	Argv []string
}

// shellCalls parses src as POSIX shell and returns each command's literal argv.
// Using a real parser (mvdan.cc/sh, as internal/lint/tool_preflight.go already
// does) means comments, quoting, pipelines, `&&` lists, backgrounding, line
// continuations and `$(…)` substitutions are handled by construction rather
// than by hand-rolled string surgery — and a command inside `$(…)` is visited
// as a command in its own right.
//
// Unparseable text yields nothing: prose fragments in backticks are not
// required to be valid shell.
func shellCalls(src string) []shellCall {
	file, err := syntax.NewParser().Parse(strings.NewReader(src), "")
	if err != nil {
		return nil
	}
	var calls []shellCall
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		argv := make([]string, 0, len(call.Args))
		for _, word := range call.Args {
			literal, ok := literalWord(word)
			if !ok {
				return true // an expansion makes this argv unresolvable
			}
			argv = append(argv, literal)
		}
		calls = append(calls, shellCall{Line: int(call.Pos().Line()), Argv: argv})
		return true
	})
	return calls
}

// literalWord returns a shell word's literal text, reporting false when it
// contains an expansion whose value is not statically known.
func literalWord(word *syntax.Word) (string, bool) {
	var b strings.Builder
	for _, part := range word.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, inner := range p.Parts {
				lit, ok := inner.(*syntax.Lit)
				if !ok {
					return "", false
				}
				b.WriteString(lit.Value)
			}
		default:
			return "", false
		}
	}
	return b.String(), true
}

// macheArgvs returns the mache invocations inside a recovered argv: the command
// itself when mache is the head, plus the `--` handoff idiom an MCP client
// registration uses (`claude mcp add mache -- mache serve --stdio .`), where
// the real mache command is nested inside another tool's argv.
func macheArgvs(argv []string) [][]string {
	var out [][]string
	if len(argv) > 0 && isMacheBinary(argv[0]) {
		out = append(out, argv)
	}
	for i, tok := range argv {
		if tok == "--" && i+1 < len(argv) && isMacheBinary(argv[i+1]) {
			out = append(out, argv[i+1:])
		}
	}
	return out
}

// isMacheBinary reports whether tok invokes mache — bare, or by any path
// (./mache, /usr/local/bin/mache).
func isMacheBinary(tok string) bool {
	return tok == "mache" || strings.HasSuffix(tok, "/mache")
}
