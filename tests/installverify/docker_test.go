package installverify

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The clean-HOME leg — bead mache-19326d's acceptance criterion in the one
// place it can be honestly met.
//
// "A user with only the released mache binary (no repo, no Taskfile) can
// provision the pinned leyline and invoke it version-correctly" is not
// testable on a dev box: the box has ~/.mache, a checkout, a Taskfile, and a
// leyline on PATH, and no amount of env scrubbing removes the possibility that
// one of them is why something worked. A container has none of them.
//
// This leg SKIPS when docker is absent or the image cannot be fetched, so it
// never reddens CI on an agent box without a daemon. Set
// MACHE_VERIFY_REQUIRE_DOCKER=1 (CI, release verification) to turn those skips
// into failures — otherwise a permanently-skipping gate is indistinguishable
// from a passing one.

const (
	imageEnv       = "MACHE_VERIFY_IMAGE"
	requireDockerE = "MACHE_VERIFY_REQUIRE_DOCKER"

	// Paths inside the published image (Dockerfile.release).
	imageMachePath   = "/usr/local/bin/mache"
	imageLeylinePath = "/usr/local/bin/leyline"

	// jsonSentinel marks where a container script's log output stops and the
	// JSON document to be parsed begins.
	jsonSentinel = "---INSTALL-VERIFY-JSON---"
)

// dockerRequired reports whether a docker skip should be a failure instead.
func dockerRequired() bool { return os.Getenv(requireDockerE) != "" }

// skipOrFail skips the test, or fails it when docker was declared required.
func skipOrFail(t *testing.T, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	if dockerRequired() {
		t.Fatalf("%s (%s is set, so this is a failure rather than a skip)", msg, requireDockerE)
	}
	t.Skipf("%s — set %s=1 to make this a failure instead", msg, requireDockerE)
}

// verifyImage is the published image under test. Defaults to the image for
// THIS tree's release version, so `task install:verify:docker` on a release
// commit checks the artifact that commit publishes.
func verifyImage(t *testing.T) string {
	t.Helper()
	if v := strings.TrimSpace(os.Getenv(imageEnv)); v != "" {
		return v
	}
	return "ghcr.io/agentic-research/mache:v" + versionTxt(t, macheTreeRoot(t))
}

// dockerAvailable reports whether a docker CLI and a live daemon are both
// present. `docker version --format {{.Server.Version}}` fails when the CLI is
// installed but the daemon is not running, which is the common agent-box case.
func dockerAvailable(t *testing.T) bool {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}
	res := runner{}.run(t, "docker", "version", "--format", "{{.Server.Version}}")
	return res.err == nil && res.code == 0
}

// ensureImage makes the image locally available, pulling once if needed.
func ensureImage(t *testing.T, image string) bool {
	t.Helper()
	inspect := runner{}.run(t, "docker", "image", "inspect", image)
	if inspect.err == nil && inspect.code == 0 {
		return true
	}
	res := runner{}.run(t, "docker", "pull", "--quiet", image)
	if res.err != nil || res.code != 0 {
		t.Logf("docker pull %s failed: %s", image, res.combined())
		return false
	}
	return true
}

// dockerImage is a fetched image plus the assertions' shared context.
type dockerImage struct {
	t     *testing.T
	image string
}

// setupDockerImage performs the skip dance once and returns a handle.
func setupDockerImage(t *testing.T) dockerImage {
	t.Helper()
	if !dockerAvailable(t) {
		skipOrFail(t, "no docker daemon available")
	}
	image := verifyImage(t)
	if !ensureImage(t, image) {
		skipOrFail(t, "image %s is not fetchable", image)
	}
	return dockerImage{t: t, image: image}
}

// runIn executes argv inside the image with an explicit entrypoint. Every
// invocation here passes --entrypoint on purpose: see
// TestPublishedImageEntrypointIsSerize below for why a bare `docker run IMAGE
// <cmd>` does not do what it looks like it does.
func (d dockerImage) runIn(entrypoint string, extraDockerArgs []string, argv ...string) result {
	d.t.Helper()
	args := []string{"run", "--rm", "--entrypoint", entrypoint}
	args = append(args, extraDockerArgs...)
	args = append(args, d.image)
	args = append(args, argv...)
	return runner{}.run(d.t, "docker", args...)
}

// TestPublishedImageEntrypointBakesInServe pins bead mache-504adc: the image's
// ENTRYPOINT already carries `serve`, so `docker run IMAGE version` is
// `mache serve version` — it does NOT print a version, it silently boots a
// default server on 7532 and blocks. Anything driving this image (README
// snippets, CI, this file) must pass --entrypoint.
//
// Asserting the entrypoint structurally, rather than describing the hazard in
// a comment, means a future change to how the image is driven either updates
// this expectation deliberately or fails here.
func TestPublishedImageEntrypointBakesInServe(t *testing.T) {
	d := setupDockerImage(t)

	res := runner{}.run(t, "docker", "image", "inspect", d.image,
		"--format", "{{json .Config.Entrypoint}}")
	require.NoError(t, res.err)
	require.Zerof(t, res.code, "docker image inspect: %s", res.combined())

	var entrypoint []string
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(res.stdout)), &entrypoint))
	assert.Equal(t, []string{imageMachePath, "serve"}, entrypoint,
		"the image bakes `serve` into ENTRYPOINT (mache-504adc) — every `docker run` of it "+
			"appends to `mache serve`, so callers must pass --entrypoint to run any other subcommand")
}

// TestPublishedImageReportsItsTagVersion is gate (b) for the OCI artifact:
// the mache inside the image must report the version its tag claims.
// Nothing verified this before (bead mache-4d7f2c).
func TestPublishedImageReportsItsTagVersion(t *testing.T) {
	d := setupDockerImage(t)

	res := d.runIn(imageMachePath, nil, "version")
	require.NoError(t, res.err)
	require.Zerof(t, res.code, "`mache version` in %s exited %d: %s", d.image, res.code, res.combined())

	token, ok := parseVersionLine(res.stdout)
	require.Truef(t, ok, "image mache printed no version line:\n%s", res.combined())

	_, tag, hasTag := strings.Cut(d.image, ":")
	if !hasTag {
		t.Skipf("image reference %q carries no tag to compare against", d.image)
	}
	assert.Equal(t, strings.TrimPrefix(tag, "v"), token,
		"the mache inside %s reports a version its tag does not claim", d.image)
}

// TestPublishedImageBundlesThePinnedLeyline asserts the leyline shipped
// alongside mache in the image is the one that mache pins. A mismatch here is
// the containerised form of the mache-19326d skew: the .db an image builds
// would be parsed by a different leyline than the one its mache expects.
//
// The comparison is only meaningful when THIS COMMIT is the one the image
// was actually built from — an unreleased commit that bumped the leyline pin
// (a normal PR, not yet a release) legitimately diverges from the last
// published image, which was built from an earlier commit. verifyImage's
// default resolves the image tag from this tree's OWN version.txt, so
// comparing d.image against that same re-derived string can never catch
// this — both sides come from the identical formula. The only real signal is
// whether HEAD is actually tagged: tagAtHEAD mirrors `task version:check`
// (mache-4d7f2c's own release-integrity invariant), so this stays consistent
// with how the sibling TestInstalledMacheReportsExpectedVersion in
// version_test.go decides the same question.
func TestPublishedImageBundlesThePinnedLeyline(t *testing.T) {
	d := setupDockerImage(t)

	res := d.runIn(imageLeylinePath, nil, "--version")
	require.NoError(t, res.err)
	require.Zerof(t, res.code, "bundled leyline --version exited %d: %s", res.code, res.combined())
	got := firstSemver(res.combined())
	require.NotEmptyf(t, got, "bundled leyline printed no version:\n%s", res.combined())

	want := expectedLeylinePin(t)
	root := macheTreeRoot(t)
	if tag := tagAtHEAD(t, root); tag == "" {
		t.Logf("HEAD is not a release tag (this tree is ahead of its last release); "+
			"image %s bundles leyline %s, this tree pins %s — not compared", d.image, got, want)
		return
	}
	assert.Equal(t, want, got, "the image bundles a leyline that is not this tree's pin")
}

// TestDockerCleanHomeProjectsAndQueries is the acceptance criterion itself:
// from a HOME with nothing in it, no repo checkout and no Taskfile, the
// published binary must project a source tree and answer a real query about it.
//
// HOME is redirected to an empty in-container path (the image's default /data
// is a declared VOLUME and could be populated by a caller), and the fixture is
// mounted read-only, so the only thing that can make this pass is the shipped
// artifact.
func TestDockerCleanHomeProjectsAndQueries(t *testing.T) {
	d := setupDockerImage(t)

	// One shell so the .db survives between commands — `--rm` discards the
	// container filesystem, so a second `docker run` would start from nothing.
	// jsonSentinel separates mache's own chatter from the JSON to be parsed:
	// splitting on a marker the script emits is deterministic, where guessing
	// where the log stops and the document starts is not.
	script := strings.Join([]string{
		"set -e",
		`test ! -e "$HOME/.mache"`, // genuinely clean to begin with
		imageMachePath + " build /source /tmp/fixture.db",
		"test -s /tmp/fixture.db", // exit 0 is not evidence
		"echo " + jsonSentinel,
		// One named rule, not `--rule '*'`: the glob form streams one JSON
		// document PER RULE, which is not a value an assertion can decode.
		imageMachePath + " find-smells --db /tmp/fixture.db --rule untested_function --format=json --fail-on=none",
	}, "\n")

	res := d.runIn("/bin/sh", []string{
		"-e", "HOME=/tmp/clean-home",
		"-v", fixtureDir(t) + ":/source:ro",
	}, "-c", script)

	require.NoError(t, res.err)
	require.Zerof(t, res.code,
		"a clean-HOME projection failed inside %s — this is exactly the state a release-asset "+
			"or Homebrew user is in\n%s", d.image, res.combined())

	// The build log names the leyline it resolved; inside the image that must
	// be the bundled one, reached with no download and no ~/.mache.
	assert.Contains(t, res.combined(), imageLeylinePath,
		"the containerised mache must resolve the bundled leyline, not go looking elsewhere")

	// And the query must return the RIGHT answer, not merely a well-formed
	// one. The fixture holds exactly two top-level functions and no tests, so
	// untested_function must name both, by their projection addresses.
	_, after, ok := strings.Cut(res.stdout, jsonSentinel)
	require.Truef(t, ok, "the script's JSON marker never appeared:\n%s", res.combined())
	body := strings.TrimSpace(after)
	require.NotEmpty(t, body, "find-smells produced no JSON:\n%s", res.combined())

	var scan struct {
		Rule     string `json:"rule"`
		Total    int    `json:"total"`
		Findings []struct {
			SourceID string `json:"source_id"`
			NodeID   string `json:"node_id"`
		} `json:"findings"`
	}
	require.NoErrorf(t, json.Unmarshal([]byte(body), &scan),
		"find-smells output is not the expected JSON shape:\n%s", body)

	assert.Equal(t, "untested_function", scan.Rule)
	nodeIDs := make([]string, 0, len(scan.Findings))
	for _, f := range scan.Findings {
		assert.Equal(t, "pkg/checksum.go", f.SourceID)
		nodeIDs = append(nodeIDs, f.NodeID)
	}
	assert.ElementsMatch(t,
		[]string{fixtureChecksumDef, "pkg/checksum.go/function_declaration_1"}, nodeIDs,
		"the containerised projection must address the fixture's two functions exactly")
	assert.Equal(t, len(scan.Findings), scan.Total, "reported total must match the findings returned")
}
