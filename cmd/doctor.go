package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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

	checks := []check{
		checkLocalBinary(),
		checkDaemon(daemonVersion, daemonErr),
		checkVersionSkew(daemonVersion, daemonErr),
		checkPinnedLeyline(),
		checkArena(cwd),
		checkProjectRegistration(cwd),
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
	return check{
		Name:   "leyline-pin",
		Status: statusOK,
		Detail: fmt.Sprintf("%s resolved at %s", leyline.BinaryVersion, path),
	}
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
	return check{
		Name:   "project",
		Status: statusFail,
		Detail: fmt.Sprintf("%s is NOT registered; MCP tools here will fail to resolve a workspace root", want),
		Fix:    "mache init   # registers this project with the shared daemon",
	}
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

	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":` +
		`{"protocolVersion":"2025-06-18","capabilities":{},` +
		`"clientInfo":{"name":"mache-doctor","version":"1"}}}`)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, macheHTTPURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return serverVersionFromMCPReply(raw)
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
