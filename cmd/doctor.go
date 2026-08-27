package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/agentic-research/mache/internal/leyline"
)

// doctorProbeTimeout bounds every network probe. A diagnostic that can hang is
// worse than no diagnostic: the incident this command exists for presented as
// three separate timeouts, and an operator debugging a timeout must never be
// made to wait on another one.
const doctorProbeTimeout = 3 * time.Second

// checkStatus is deliberately three-valued. "warn" is for states that are
// legitimate but worth seeing (no daemon running when you did not ask for one);
// only "fail" sets the exit code, so `mache doctor` in a script means "is
// anything actually broken", not "is anything unusual".
type checkStatus string

const (
	statusOK   checkStatus = "ok"
	statusWarn checkStatus = "warn"
	statusFail checkStatus = "fail"
)

// check is one diagnostic result. Fix is mandatory on a fail — a check that can
// report a problem without naming the remediation just relocates the guessing.
type check struct {
	Name   string      `json:"name"`
	Status checkStatus `json:"status"`
	Detail string      `json:"detail"`
	Fix    string      `json:"fix,omitempty"`
}

var doctorJSON bool

func init() {
	doctorCmd.Flags().BoolVar(&doctorJSON, "json", false, "Emit results as JSON (agent-readable)")
	rootCmd.AddCommand(doctorCmd)
}

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Diagnose the mache daemon, arena, and project registration",
	Long: `Reports whether mache is actually able to serve this directory.

Every check names its own remediation. Exits non-zero when any check fails, so
it works as a gate as well as a report.

Checks: local binary version; daemon liveness and version; version skew between
the two; pinned ley-line resolvability; shared-arena binding; and whether this
working directory is registered with the daemon.`,
	RunE:         runDoctor,
	SilenceUsage: true,
	// cmd.Execute already prints the returned error; without this cobra
	// prints it too and every failure is reported twice.
	SilenceErrors: true,
}

func runDoctor(cmd *cobra.Command, _ []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}

	daemonVersion, daemonErr := probeDaemonVersion(cmd.Context())

	root := workspaceRootFor(cwd)

	checks := []check{
		checkLocalBinary(),
		checkDaemon(daemonVersion, daemonErr),
		checkVersionSkew(daemonVersion, daemonErr),
		checkPinnedLeyline(),
		checkArena(root),
		checkProjectRegistration(root),
		checkClientToken(root),
	}

	if doctorJSON {
		return emitDoctorJSON(cmd.OutOrStdout(), checks)
	}
	return emitDoctorText(cmd.OutOrStdout(), checks)
}

// checkLocalBinary is informational and never fails: it is the reference point
// every other version statement is relative to.
func checkLocalBinary() check {
	return check{
		Name:   "binary",
		Status: statusOK,
		Detail: fmt.Sprintf("mache %s (this executable)", Version),
	}
}

func checkDaemon(version string, err error) check {
	if err != nil {
		return check{
			Name:   "daemon",
			Status: statusWarn,
			Detail: fmt.Sprintf("no MCP daemon answering at %s (%v)", macheHTTPURL, err),
			Fix:    "mache init --global   # installs and starts the shared HTTP daemon",
		}
	}
	return check{
		Name:   "daemon",
		Status: statusOK,
		Detail: fmt.Sprintf("answering at %s, reports version %s", macheHTTPURL, version),
	}
}

// checkVersionSkew is the check that would have caught the reported incident:
// two stale daemons kept serving after the binary was upgraded, and nothing
// compared them. mache already advertises its version over MCP
// (server.NewMCPServer("mache", Version, ...)); nothing consumed it.
func checkVersionSkew(daemonVersion string, err error) check {
	if err != nil {
		return check{
			Name:   "version-skew",
			Status: statusWarn,
			Detail: "cannot compare: no daemon answered",
		}
	}
	if buildVersion == "" {
		return check{
			Name:   "version-skew",
			Status: statusWarn,
			Detail: fmt.Sprintf("cannot compare reliably: this is a bare build reporting the release base %q, not its real git distance (daemon reports %s)", Version, daemonVersion),
			Fix:    "compare against an installed build: task build && ./mache doctor",
		}
	}
	if daemonVersion == Version {
		return check{
			Name:   "version-skew",
			Status: statusOK,
			Detail: fmt.Sprintf("daemon and binary agree (%s)", Version),
		}
	}
	return check{
		Name:   "version-skew",
		Status: statusFail,
		Detail: fmt.Sprintf("daemon is %s but this binary is %s — the running daemon is serving OLD code", daemonVersion, Version),
		Fix:    "launchctl kickstart -k gui/$(id -u)/com.agentic-research.mache",
	}
}

// checkPinnedLeyline refuses to download. A diagnostic must report the world as it
// is, not change it — and an operator running doctor to explain a failure would
// not expect it to start fetching binaries.
func checkPinnedLeyline() check {
	path, err := leyline.ResolveBinary(false)
	if err != nil {
		return check{
			Name:   "leyline-pin",
			Status: statusFail,
			Detail: fmt.Sprintf("pinned ley-line %s is not resolvable without downloading: %v", leyline.BinaryVersion, err),
			Fix:    "mache install   # fetches and SHA-verifies the pinned release",
		}
	}
	// The resolved BINARY is only half the answer. A daemon already running was
	// adopted, not spawned, so the exact pin ResolveBinary enforces never
	// applied to it — this check reported `ok v0.19.0` while saying nothing
	// about what was actually serving. Accurate about what it measured, silent
	// about what the reader cares about (mache-233902).
	//
	// Undeterminable is not a failure: no daemon running, or one predating the
	// leyline_version op, are both normal.
	if got, ok := leyline.AdoptedDaemonVersion(); ok && !sameSemver(got, leyline.BinaryVersion) {
		return check{
			Name:   "leyline-pin",
			Status: statusWarn,
			Detail: fmt.Sprintf("binary %s resolved at %s, but the RUNNING daemon reports %s — "+
				"_ast output may differ from CI (ley-line-open ships schema changes in patch releases)",
				leyline.BinaryVersion, path, got),
			Fix: "mache daemon restart   # or stop the stale leyline daemon so the pinned one is spawned",
		}
	}
	// Provenance, when a spawn recorded it (mache-967cff). Absence is normal —
	// pre-record daemons and hand-started ones have none — but a record that
	// says the spawner is DEAD names the orphaned-but-alive state that used to
	// accumulate silently: an 11-day daemon was findable only via `ps etime`,
	// and a leaked test daemon was detected by drift but attributable by
	// nothing.
	if rec, ok := leyline.WellKnownOwnerRecord(); ok {
		age := rec.Age(time.Now()).Round(time.Minute)
		switch {
		case rec.Orphaned():
			return check{
				Name:   "leyline-pin",
				Status: statusWarn,
				Detail: fmt.Sprintf("%s resolved at %s; the running daemon (pid %d, up %s, pin %s) has OUTLIVED its spawner (pid %d, dead) — nothing owns it",
					leyline.BinaryVersion, path, rec.DaemonPID, age, rec.Pin, rec.SpawnerPID),
				Fix: "mache daemon restart   # or kill the orphan so the next mache spawns a fresh pinned daemon",
			}
		case rec.Stale():
			return check{
				Name:   "leyline-pin",
				Status: statusOK,
				Detail: fmt.Sprintf("%s resolved at %s (a stale owner record names dead pid %d — file outlived the daemon; harmless, rewritten on next spawn)",
					leyline.BinaryVersion, path, rec.DaemonPID),
			}
		default:
			return check{
				Name:   "leyline-pin",
				Status: statusOK,
				Detail: fmt.Sprintf("%s resolved at %s; daemon pid %d up %s, spawned by pid %d with pin %s",
					leyline.BinaryVersion, path, rec.DaemonPID, age, rec.SpawnerPID, rec.Pin),
			}
		}
	}
	return check{
		Name:   "leyline-pin",
		Status: statusOK,
		Detail: fmt.Sprintf("%s resolved at %s", leyline.BinaryVersion, path),
	}
}

// sameSemver compares two version strings ignoring a leading "v", so a daemon
// reporting "0.19.0" matches a pin written "v0.19.0".
func sameSemver(a, b string) bool {
	return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
}

// checkArena surfaces the warm-start refusal that previously showed up only as
// daemon-backed features going quiet. leyline binds its living database to the
// tree it parsed; serving a second project from the one shared arena makes it
// refuse to warm-start, and every DiscoverOrStart caller degrades rather than
// propagating, so the operator sees silence instead of a reason.
func checkArena(cwd string) check {
	st, err := leyline.InspectArena()
	if err != nil {
		return check{
			Name:   "arena",
			Status: statusWarn,
			Detail: fmt.Sprintf("cannot inspect arena: %v", err),
		}
	}
	switch {
	case !st.Exists:
		return check{
			Name:   "arena",
			Status: statusOK,
			Detail: fmt.Sprintf("no arena yet at %s (cold start; nothing to conflict with)", st.Path),
		}
	case !st.HasRecord:
		return check{
			Name:   "arena",
			Status: statusWarn,
			Detail: fmt.Sprintf("arena exists at %s but mache has no record of how it was spawned; the next serve will reset it", st.Path),
		}
	case st.ArenaBoundElsewhere(cwd):
		return check{
			Name:   "arena",
			Status: statusFail,
			Detail: fmt.Sprintf("arena is bound to %s but you are in %s — ley-line will refuse to warm-start", st.SourceRoot, cwd),
			Fix:    "the next `mache serve` here resets it automatically; to force it now, remove " + st.Path,
		}
	case st.SourceRoot == "":
		return check{
			Name:   "arena",
			Status: statusOK,
			Detail: fmt.Sprintf("not bound to any tree (spawned without --source, serving a pre-baked .db); cdc target %q", st.CDCTarget),
		}
	default:
		return check{
			Name:   "arena",
			Status: statusOK,
			Detail: fmt.Sprintf("bound to %s (cdc target %q)", st.SourceRoot, st.CDCTarget),
		}
	}
}

// checkProjectRegistration answers the question behind "workspace root
// unavailable (context deadline exceeded)" — a message that describes the
// symptom (a timeout) rather than the cause (this directory was never
// registered, so no token resolves to it).
func checkProjectRegistration(cwd string) check {
	reg, err := loadProjectRegistry()
	if err != nil {
		return check{
			Name:   "project",
			Status: statusFail,
			Detail: fmt.Sprintf("cannot read the project registry: %v", err),
			Fix:    "mache init   # re-registers this project",
		}
	}
	want := leyline.CanonicalSourceRoot(cwd)
	for _, p := range reg {
		if leyline.CanonicalSourceRoot(p) == want {
			return check{
				Name:   "project",
				Status: statusOK,
				Detail: fmt.Sprintf("%s is registered (%d project(s) total)", want, len(reg)),
			}
		}
	}
	// NOT a failure. Registration is an optimization, not a prerequisite: the
	// daemon asks the connecting client for its workspace root over
	// roots/list, and only falls back to the ?project= token for clients that
	// cannot answer. Claude Code answers — verified against a live daemon,
	// where a session resolved /Users/.../ley-line-open with no registry entry
	// and no .claude/mcp.json at all.
	//
	// This check previously said "MCP tools here will fail to resolve a
	// workspace root" and returned statusFail, which made `mache doctor` exit
	// 1 on a perfectly healthy tree and sent people to run `mache init` per
	// directory — and, because a git worktree is its own path, per BRANCH. The
	// claim was false for exactly the client most users have.
	//
	// doctor cannot know which client will connect, so it cannot know whether
	// the token is needed. Report the fact and let the reader decide.
	return check{
		Name:   "project",
		Status: statusWarn,
		Detail: fmt.Sprintf("%s is not pre-registered; clients that answer roots/list (Claude Code does) "+
			"resolve it anyway, and the daemon registers it on first use", want),
		Fix: "mache init   # only needed for clients that cannot answer roots/list",
	}
}

// mcpClientConfigs are the per-project files an MCP client reads, in the order
// doctor reports them. The user-scope config is deliberately excluded: doctor
// cannot know which client is asking, and a global entry that lacks a token is
// correct for clients that answer roots/list.
var mcpClientConfigs = []string{
	filepath.Join(".claude", "mcp.json"),
	".mcp.json",
}

// checkClientToken closes the gap that made the `project` check give false
// confidence: a project can be REGISTERED with the daemon while the client
// still cannot reach it.
//
// Observed during validation — doctor reported six green checks while
// find_definition returned "workspace root unavailable (context deadline
// exceeded)", because the client's URL was the bare endpoint and its
// roots/list never answered. Registration and reachability are different
// questions, and only the first was being asked.
//
// A missing token is a WARN, not a fail: a client that answers roots/list
// resolves without one. It is reported because when roots/list does NOT
// answer, this is the difference between working and timing out — and the
// timeout says nothing about the cause.
func checkClientToken(root string) check {
	var withToken, without []string
	for _, rel := range mcpClientConfigs {
		url, ok := macheURLIn(filepath.Join(root, rel))
		if !ok {
			continue
		}
		if strings.Contains(url, "project=") {
			withToken = append(withToken, rel)
		} else {
			without = append(without, rel)
		}
	}
	switch {
	case len(withToken) == 0 && len(without) == 0:
		return check{
			Name:   "client-token",
			Status: statusWarn,
			Detail: "no per-project MCP client config found; a client must answer roots/list to resolve this workspace",
			Fix:    "mache init   # writes .claude/mcp.json with a ?project= token",
		}
	case len(withToken) > 0:
		return check{
			Name:   "client-token",
			Status: statusOK,
			Detail: fmt.Sprintf("%s carries a ?project= token", strings.Join(withToken, ", ")),
		}
	default:
		return check{
			Name:   "client-token",
			Status: statusWarn,
			// Two costs, not one. Resolution: a bare URL depends on the client
			// answering roots/list. Continuity: a ?project= session re-binds
			// STATELESSLY after a daemon restart (the URL itself carries the
			// binding; verified live in mache-956488), while a roots-bound
			// session is severed by every upgrade and stalls or errors until
			// the client reconnects. The token cannot be committed to a shared
			// config — it is salted per machine — so this is a per-machine step.
			Detail: fmt.Sprintf("no ?project= token in %s; tools resolve only if your client answers roots/list, "+
				"and sessions will NOT survive daemon restarts/upgrades (a ?project= URL re-binds statelessly)",
				strings.Join(without, ", ")),
			Fix: "mache init   # writes .claude/mcp.json with a ?project= token (per-machine; do not commit the token)",
		}
	}
}

// macheURLIn reads one MCP client config and returns the mache server's URL.
// A missing or unparseable file is simply "no opinion" — doctor describes what
// it finds and does not editorialise about files it cannot read.
func macheURLIn(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var cfg struct {
		MCPServers map[string]struct {
			URL string `json:"url"`
		} `json:"mcpServers"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return "", false
	}
	entry, ok := cfg.MCPServers["mache"]
	if !ok || entry.URL == "" {
		return "", false
	}
	return entry.URL, true
}

// workspaceRootFor walks up from dir to the enclosing repository root, because
// that — not the process working directory — is what actually gets registered.
// MCP clients advertise a workspace root and mache registers THAT; asking
// whether a subdirectory is registered answers a question nobody posed.
//
// Getting this wrong is worse than a false alarm: the remediation would tell an
// operator to run `mache init` inside e.g. cmd/, minting a SECOND token for a
// nested path and making the registry genuinely wrong.
//
// A `.git` entry may be a directory (normal clone) or a file (worktree or
// submodule, holding a gitdir pointer); either marks the root. Falls back to
// dir when nothing above it looks like a repository.
func workspaceRootFor(dir string) string {
	cur := dir
	for {
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return dir
		}
		cur = parent
	}
}

// newInitializeRequest builds the MCP handshake probe. Split out so
// probeDaemonVersion reads as what it is — dispatch, session hygiene, parse —
// rather than interleaving request construction with all three.
func newInitializeRequest(ctx context.Context) (*http.Request, error) {
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":` +
		`{"protocolVersion":"2025-06-18","capabilities":{},` +
		`"clientInfo":{"name":"mache-doctor","version":"1"}}}`)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, macheHTTPURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	return req, nil
}

// probeDaemonVersion performs a real MCP initialize and reads serverInfo.version.
// A plain TCP connect would prove only that something holds the port — which is
// exactly the wrong answer when the problem is a stale daemon still listening.
func probeDaemonVersion(parent context.Context) (string, error) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, doctorProbeTimeout)
	defer cancel()

	req, err := newInitializeRequest(ctx)
	if err != nil {
		return "", err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	// Streamable HTTP is stateful: initialize ALLOCATES a server-side session.
	// Without this, every `mache doctor` run leaks one — and a diagnostic
	// people are encouraged to run in a loop or a gate is the worst possible
	// place to leak a resource. Terminating is best-effort: a daemon that
	// ignores DELETE still sweeps idle sessions, so failing to clean up must
	// not turn a healthy probe into a reported failure.
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		defer releaseMCPSession(parent, sid)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return serverVersionFromMCPReply(raw)
}

// releaseMCPSession terminates the session initialize allocated. Errors are
// deliberately swallowed: this is hygiene, not a health signal, and a daemon
// that refuses DELETE is not thereby unhealthy.
func releaseMCPSession(parent context.Context, sessionID string) {
	ctx, cancel := context.WithTimeout(parent, doctorProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, macheHTTPURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("Mcp-Session-Id", sessionID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}

// serverVersionFromMCPReply tolerates both response framings the transport may
// use: a bare JSON body, or Server-Sent Events where the payload rides on a
// `data:` line. Accepting only one would make doctor's verdict depend on a
// transport detail rather than on daemon health.
func serverVersionFromMCPReply(raw []byte) (string, error) {
	payload := bytes.TrimSpace(raw)
	if !bytes.HasPrefix(payload, []byte("{")) {
		for line := range strings.SplitSeq(string(raw), "\n") {
			if after, ok := strings.CutPrefix(strings.TrimSpace(line), "data:"); ok {
				payload = []byte(strings.TrimSpace(after))
				break
			}
		}
	}
	var reply struct {
		Result struct {
			ServerInfo struct {
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(payload, &reply); err != nil {
		return "", fmt.Errorf("unreadable MCP reply: %w", err)
	}
	if reply.Result.ServerInfo.Version == "" {
		return "", fmt.Errorf("MCP reply carried no serverInfo.version")
	}
	return reply.Result.ServerInfo.Version, nil
}

func emitDoctorJSON(w io.Writer, checks []check) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(struct {
		Checks []check `json:"checks"`
		Failed int     `json:"failed"`
	}{checks, countFailures(checks)}); err != nil {
		return err
	}
	return doctorExit(checks)
}

func emitDoctorText(w io.Writer, checks []check) error {
	for _, c := range checks {
		mark := map[checkStatus]string{statusOK: "ok  ", statusWarn: "warn", statusFail: "FAIL"}[c.Status]
		_, _ = fmt.Fprintf(w, "%s  %-14s %s\n", mark, c.Name, c.Detail)
		if c.Fix != "" {
			_, _ = fmt.Fprintf(w, "      %-14s → %s\n", "", c.Fix)
		}
	}
	return doctorExit(checks)
}

func countFailures(checks []check) int {
	n := 0
	for _, c := range checks {
		if c.Status == statusFail {
			n++
		}
	}
	return n
}

// doctorExit returns an error (hence a non-zero exit) when anything failed, so
// the command is usable as a gate. The message is deliberately terse: the
// per-check lines above already carry the detail and the remediation.
func doctorExit(checks []check) error {
	if n := countFailures(checks); n > 0 {
		return fmt.Errorf("%d check(s) failed", n)
	}
	return nil
}
