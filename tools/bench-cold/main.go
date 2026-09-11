// Command bench-cold measures mache's cold path — the first `mache build` on
// a repo — against the committed envelope in testdata/snapshots/cold-budget.toml.
//
// Bead mache-2de6c0. The framing is a person who just downloaded mache and
// pointed it at their code on a 4-core/16 GB laptop with an IDE already open.
// Setup-to-usage is a product property, and until it is measured on a pinned
// corpus with the numbers committed, "it's fast enough" is an opinion.
//
// Two phases are measured:
//
//	leyline parse  — the intermediate artifact, measured on its own so the
//	                 parse half of the cost is attributable
//	mache build    — what the user actually runs; it re-runs the parse
//	                 internally, so its wall time INCLUDES the phase above
//	                 and the two are NOT additive
//
// The gate is on bytes, not milliseconds: peak RSS and both artifact sizes
// are hard limits, wall time is reported against an advisory target. Bytes
// are comparable across machines; milliseconds are not.
//
// Usage:
//
//	task bench:cold                      # pinned corpus = this repo at HEAD
//	go run ./tools/bench-cold --corpus /path/to/repo
//	go run ./tools/bench-cold --json
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentic-research/mache/internal/benchrun"
	"github.com/agentic-research/mache/internal/leyline"
	"github.com/agentic-research/mache/internal/projcfg"
)

func main() {
	opts := parseFlags()
	if err := benchColdPath(opts); err != nil {
		fmt.Fprintf(os.Stderr, "bench-cold: %v\n", err)
		os.Exit(2)
	}
}

// report is the machine-readable form of one bench run (--json).
type report struct {
	CorpusSHA         string   `json:"corpus_sha"`
	Corpus            string   `json:"corpus"`
	Files             int      `json:"files"`
	CorpusBytes       int64    `json:"corpus_bytes"`
	GOMAXPROCS        int      `json:"gomaxprocs"`
	Schema            string   `json:"schema"`
	LeylineVersion    string   `json:"leyline_version"`
	ParseWallMs       int64    `json:"parse_wall_ms"`
	ParsePeakRSSBytes int64    `json:"parse_peak_rss_bytes"`
	LeylineDBBytes    int64    `json:"leyline_db_bytes"`
	BuildWallMs       int64    `json:"build_wall_ms"`
	BuildPeakRSSBytes int64    `json:"build_peak_rss_bytes"`
	ProjectionDBBytes int64    `json:"projection_db_bytes"`
	PeakRSSBytes      int64    `json:"peak_rss_bytes"`
	Violations        []string `json:"violations"`
}

// options is everything the command line can change about a run.
type options struct {
	corpus     string
	budget     string
	mache      string
	schema     string
	gomaxprocs int
	asJSON     bool
	quiet      bool
}

func parseFlags() options {
	var o options
	flag.StringVar(&o.corpus, "corpus", "", "directory to build (default: this repo at HEAD, staged from `git archive`)")
	flag.StringVar(&o.budget, "budget", "", "budget file (default: testdata/snapshots/cold-budget.toml)")
	flag.StringVar(&o.mache, "mache", "bin/mache", "mache binary to measure")
	flag.StringVar(&o.schema, "schema", "", "schema preset or path (default: whatever `mache init` would auto-detect for the corpus)")
	flag.IntVar(&o.gomaxprocs, "gomaxprocs", 4, "GOMAXPROCS for the measured build — the envelope is a 4-core laptop")
	flag.BoolVar(&o.asJSON, "json", false, "emit the run as JSON instead of a table")
	flag.BoolVar(&o.quiet, "quiet", false, "discard the measured commands' own output")
	flag.Parse()
	return o
}

// corpusInfo is what was measured, and what identifies it. A wall number
// without the corpus that produced it is not comparable to any other run.
type corpusInfo struct {
	dir   string
	name  string
	sha   string
	files int
	bytes int64
}

// benchColdPath measures one cold path and checks it against the budget.
func benchColdPath(o options) error {
	root, err := macheRepoRoot()
	if err != nil {
		return err
	}
	budgetPath := o.budget
	if budgetPath == "" {
		budgetPath = filepath.Join(root, "testdata", "snapshots", "cold-budget.toml")
	}
	budget, err := benchrun.LoadBudget(budgetPath)
	if err != nil {
		return err
	}

	work, err := os.MkdirTemp("", "mache-bench-cold-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(work) }()

	corpus, err := resolveCorpus(root, work, o)
	if err != nil {
		return err
	}
	r, observed, err := measureColdPath(root, work, corpus, o)
	if err != nil {
		return err
	}

	violations := budget.Check(observed)
	for _, v := range violations {
		r.Violations = append(r.Violations, v.String())
	}
	if o.asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(r); err != nil {
			return err
		}
	} else {
		printTable(r, budget, observed)
	}
	if len(violations) > 0 {
		return fmt.Errorf("%d budget violation(s); raising a limit in %s is not how this goes green "+
			"— see the file header", len(violations), budgetPath)
	}
	return nil
}

// resolveCorpus settles what is being measured: the caller's directory, or
// this repo staged at HEAD.
func resolveCorpus(root, work string, o options) (corpusInfo, error) {
	c := corpusInfo{dir: o.corpus, name: filepath.Base(o.corpus)}
	if c.dir == "" {
		c.dir = filepath.Join(work, "corpus")
		c.name = "mache-self"
		sha, err := stageHEAD(root, c.dir)
		if err != nil {
			return corpusInfo{}, err
		}
		c.sha = sha
	}
	files, bytes, err := corpusSize(c.dir)
	if err != nil {
		return corpusInfo{}, err
	}
	c.files, c.bytes = files, bytes
	return c, nil
}

// measureColdPath runs the two phases and reports what they cost.
//
// They are measured separately so the parse half is attributable, but the
// second one re-runs the parse internally: `mache build`'s wall INCLUDES the
// first phase and the two are not additive. Peak RSS is the MAX of the two,
// not the sum, because they are sequential.
func measureColdPath(root, work string, c corpusInfo, o options) (report, benchrun.Observed, error) {
	leylineBin, err := leyline.ResolveBinary(false)
	if err != nil {
		return report{}, benchrun.Observed{}, fmt.Errorf("pinned leyline unavailable (%v); run `task leyline:ensure` first — "+
			"a cold-path number measured with a different parser is not comparable to the committed one", err)
	}
	leyline.RecordResolved(leylineBin, "resolved")
	prov, _ := leyline.Provenance()

	schema, err := resolveSchema(c.dir, o.schema)
	if err != nil {
		return report{}, benchrun.Observed{}, err
	}
	macheBin, err := resolveMacheBinary(root, o.mache)
	if err != nil {
		return report{}, benchrun.Observed{}, err
	}

	out := io.Writer(os.Stderr)
	if o.quiet {
		out = io.Discard
	}
	// GOMAXPROCS caps mache's Go parallelism. leyline is a separate binary and
	// its thread pool is NOT capped by this — the number below is the envelope
	// mache honours, not a simulated 4-core machine. Simulating one needs a
	// cgroup (Linux) or a VM; it is not something an env var can claim.
	env := []string{fmt.Sprintf("GOMAXPROCS=%d", o.gomaxprocs)}

	leylineDB := filepath.Join(work, "leyline.db")
	parse, err := benchrun.Run(out, env, leylineBin, "parse", c.dir, "-o", leylineDB)
	if err != nil {
		return report{}, benchrun.Observed{}, err
	}
	leylineBytes, err := benchrun.FileBytes(leylineDB)
	if err != nil {
		return report{}, benchrun.Observed{}, err
	}

	projectionDB := filepath.Join(work, "projection.db")
	build, err := benchrun.Run(out, env, macheBin, "build", "--schema", schema, c.dir, projectionDB)
	if err != nil {
		return report{}, benchrun.Observed{}, err
	}
	projectionBytes, err := benchrun.FileBytes(projectionDB)
	if err != nil {
		return report{}, benchrun.Observed{}, err
	}

	peak := parse.PeakRSS
	if build.PeakRSS > peak {
		peak = build.PeakRSS
	}
	r := report{
		CorpusSHA: c.sha, Corpus: c.name, Files: c.files, CorpusBytes: c.bytes,
		GOMAXPROCS: o.gomaxprocs, Schema: schema, LeylineVersion: prov.Version,
		ParseWallMs: parse.Wall.Milliseconds(), ParsePeakRSSBytes: parse.PeakRSS, LeylineDBBytes: leylineBytes,
		BuildWallMs: build.Wall.Milliseconds(), BuildPeakRSSBytes: build.PeakRSS, ProjectionDBBytes: projectionBytes,
		PeakRSSBytes: peak,
	}
	return r, benchrun.Observed{
		PeakRSSBytes:      peak,
		LeylineDBBytes:    leylineBytes,
		ProjectionDBBytes: projectionBytes,
		Wall:              build.Wall,
	}, nil
}

// resolveSchema settles which projection is measured. The measured command is
// what `mache init` sets a project up to run: a schema-projected build.
// `mache build` with NO --schema is a different product path — it copies the
// whole leyline parse db to the output and projects one node per AST node into
// it, which for this repo is 1.86 GB against 78 MB. Benching that would be
// measuring something no code user is pointed at, so the preset is resolved
// the way init resolves it and an undetectable corpus is an ERROR rather than
// a silent fallback to the other path.
func resolveSchema(corpusDir, override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if schema := projcfg.DetectProjectType(corpusDir); schema != "" {
		return schema, nil
	}
	return "", fmt.Errorf("no schema preset detected for %s; pass --schema. "+
		"Building without one measures the _ast projection, which is not the path "+
		"`mache init` sets up", corpusDir)
}

// resolveMacheBinary locates the binary under measurement.
func resolveMacheBinary(root, path string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("%s: %w (run `task build` first)", path, err)
	}
	return path, nil
}

func printTable(r report, budget benchrun.Budget, o benchrun.Observed) {
	w := os.Stdout
	_, _ = fmt.Fprintf(w, "\ncold path — %s @ %s, %d files / %s, schema %s, GOMAXPROCS=%d, leyline %s\n\n",
		r.Corpus, shortSHA(r.CorpusSHA), r.Files, benchrun.HumanBytes(r.CorpusBytes), r.Schema, r.GOMAXPROCS, r.LeylineVersion)
	_, _ = fmt.Fprintf(w, "  %-16s %10s %12s %12s\n", "phase", "wall", "peak RSS", "db")
	_, _ = fmt.Fprintf(w, "  %-16s %10s %12s %12s\n", "leyline parse",
		roundSec(r.ParseWallMs), benchrun.HumanBytes(r.ParsePeakRSSBytes), benchrun.HumanBytes(r.LeylineDBBytes))
	_, _ = fmt.Fprintf(w, "  %-16s %10s %12s %12s\n", "mache build",
		roundSec(r.BuildWallMs), benchrun.HumanBytes(r.BuildPeakRSSBytes), benchrun.HumanBytes(r.ProjectionDBBytes))
	_, _ = fmt.Fprintf(w, "\n  mache build re-runs the parse: its wall includes the row above, they are not additive.\n\n")

	_, _ = fmt.Fprintf(w, "  %-16s %12s %12s   %s\n", "metric", "measured", "budget", "verdict")
	line := func(name string, got, limit int64) {
		verdict := "ok"
		if got > limit {
			verdict = fmt.Sprintf("OVER by %s", benchrun.HumanBytes(got-limit))
		}
		_, _ = fmt.Fprintf(w, "  %-16s %12s %12s   %s\n", name, benchrun.HumanBytes(got), benchrun.HumanBytes(limit), verdict)
	}
	line("peak RSS", o.PeakRSSBytes, budget.PeakRSSBytes)
	line("leyline db", o.LeylineDBBytes, budget.LeylineDBBytes)
	line("projection db", o.ProjectionDBBytes, budget.ProjectionDBBytes)
	advisory := "ok"
	if o.Wall > time.Duration(budget.WallMsAdvisory)*time.Millisecond {
		advisory = "over (advisory — never fails the run)"
	}
	_, _ = fmt.Fprintf(w, "  %-16s %12s %12s   %s\n\n", "wall",
		roundSec(o.Wall.Milliseconds()), roundSec(int64(budget.WallMsAdvisory)), advisory)

	_, _ = fmt.Fprintf(w, "  record this run by replacing the [[measurement]] block in cold-budget.toml:\n\n")
	_, _ = fmt.Fprintf(w, "[[measurement]]\ndate = %q\nmachine_class = \"<fill in>\"\ngomaxprocs = %d\n"+
		"schema = %q\nleyline_version = %q\ncorpus = %q\ncorpus_sha = %q\nfiles = %d\nwall_ms = %d\n"+
		"peak_rss_bytes = %d\nleyline_db_bytes = %d\nprojection_db_bytes = %d\nnote = \"<what changed>\"\n\n",
		time.Now().Format("2006-01-02"), r.GOMAXPROCS, r.Schema, r.LeylineVersion, r.Corpus, r.CorpusSHA,
		r.Files, r.BuildWallMs, r.PeakRSSBytes, r.LeylineDBBytes, r.ProjectionDBBytes)
}

func roundSec(ms int64) string { return fmt.Sprintf("%.1f s", float64(ms)/1000) }

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	if sha == "" {
		return "-"
	}
	return sha
}

// repoRoot is the mache checkout this tool was invoked from.
func macheRepoRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("locate repo root: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// stageHEAD extracts the repo's tracked files at HEAD into dir and returns the
// SHA. Tracked-files-only and content-addressed by that SHA: a bench number is
// only comparable to another if the corpus was identical, and a working tree
// with a scratch file in it is not the same corpus. Same mechanism the smell
// gate uses.
func stageHEAD(root, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	shaOut, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("resolve HEAD: %w", err)
	}
	archive := exec.Command("git", "-C", root, "archive", "HEAD")
	untar := exec.Command("tar", "-x", "-C", dir)
	pipe, err := archive.StdoutPipe()
	if err != nil {
		return "", err
	}
	untar.Stdin = pipe
	untar.Stderr = os.Stderr
	if err := archive.Start(); err != nil {
		return "", err
	}
	if err := untar.Run(); err != nil {
		return "", fmt.Errorf("extract corpus: %w", err)
	}
	if err := archive.Wait(); err != nil {
		return "", fmt.Errorf("git archive: %w", err)
	}
	return strings.TrimSpace(string(shaOut)), nil
}

// corpusSize counts the regular files under dir and their total bytes, so a
// wall number is always reported next to the size of what produced it.
func corpusSize(dir string) (int, int64, error) {
	var files int
	var bytes int64
	err := filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			files++
			bytes += info.Size()
		}
		return nil
	})
	return files, bytes, err
}
