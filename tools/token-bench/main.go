// Command token-bench measures mache's token-efficiency claim: how many bytes
// reach an agent's context to answer a construct-level question, comparing
// file-granular retrieval (arm A, the built-in Read tool) against
// construct-granular retrieval (arm B, mache find_definition / read_file).
//
// Bead mache-544659. This is the BASELINE half of the falsification design —
// it must run BEFORE any projection change lands (mache-qzsk), or there is
// nothing to compare a post-change number against.
//
// What this tool measures deterministically:
//
//	F3  tokens-to-answer  — arm A vs arm B bytes for the same question
//	F4  generality        — the same ratio broken out per language/extension
//
// What it deliberately does NOT measure, because both need a live agent loop
// and this tool must stay deterministic and CI-runnable:
//
//	F1  escalation rate   — how often a construct-granular answer is followed
//	                        by a full Read of the same file. Measured instead
//	                        from session transcripts; see the bead.
//	F2  completion rate   — held as a CONTROL. A token reduction is meaningless
//	                        unless task success is unchanged; entire-graph's
//	                        published comparison holds exactly this constant.
//
// Reporting an arm-B win without F1 and F2 would be the failure this bench
// exists to prevent: returning less is trivially achievable by returning less.
package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

// Result is the emitted baseline record. Stable field names — a later run
// diffs against a committed copy of this struct.
type BenchResult struct {
	DB              string            `json:"db"`
	Constructs      int               `json:"constructs"`
	Files           int               `json:"files"`
	ArmABytes       int64             `json:"arm_a_bytes"` // whole-file retrieval, incl. cat -n prefixes
	ArmWBytes       int64             `json:"arm_w_bytes"` // bounded N-line window (pgr baseline's control)
	ArmWAvg         int64             `json:"arm_w_avg"`
	WindowLines     int               `json:"window_lines"`
	RatioWMed       float64           `json:"ratio_w_median"`  // whole-file / window — how much a line cap alone buys
	RatioBWMed      float64           `json:"ratio_bw_median"` // window / construct — mache's marginal win over a line cap
	ArmBBytes       int64             `json:"arm_b_bytes"`     // construct-granular retrieval
	ArmAAvg         int64             `json:"arm_a_avg"`       // bytes per file
	ArmBAvg         int64             `json:"arm_b_avg"`       // bytes per construct
	Ratio           float64           `json:"ratio"`           // ratio-of-means: arm_a_avg / arm_b_avg
	RatioMed        float64           `json:"ratio_median"`    // median PAIRED ratio — the headline statistic
	RatioMean       float64           `json:"ratio_mean"`      // mean paired ratio (outlier-sensitive)
	RatioP25        float64           `json:"ratio_p25"`
	RatioP75        float64           `json:"ratio_p75"`
	Breakeven       float64           `json:"breakeven"` // constructs/file — above this, arm A wins
	MaxPerFile      int               `json:"max_constructs_per_file"`
	PerExt          map[string]ExtRow `json:"per_ext"` // F4
	Ceiling         *Ceiling          `json:"ceiling,omitempty"`
	Containment     []Containment     `json:"containment"`
	MedianFileLines int               `json:"median_file_lines"`
	Caveats         []string          `json:"caveats"`
	Warnings        []string          `json:"warnings,omitempty"`
}

// armACost is what the built-in Read tool actually puts in context: the file
// bytes PLUS cat -n line-number prefixes ("%d\t" per line). Charging raw file
// size understates arm A by ~18% on typical source, which understates the
// win — measured, not assumed.
func armACost(path string) (int64, error) {
	n, _, err := fileCost(path)
	return n, err
}

// fileCost returns (whole-file cost, line count). Cost is file bytes plus the
// cat -n prefixes Read emits.
func fileCost(path string) (int64, int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	lines := bytes.Count(b, []byte{'\n'})
	if len(b) > 0 && b[len(b)-1] != '\n' {
		lines++ // trailing line without a newline still gets numbered
	}
	overhead := 0
	for i := 1; i <= lines; i++ {
		overhead += len(strconv.Itoa(i)) + 1 // number + tab
	}
	return int64(len(b) + overhead), lines, nil
}

// armWCost models a BOUNDED window read — pgr's baseline control, which caps
// at max_lines=80 by default. This is the honest null hypothesis for mache on
// the token axis: a line cap is free, needs no ingestion, is language-agnostic,
// and works on Rust today. If it captures most of the ceiling, mache's token
// argument is worth only the remainder.
func armWCost(path string, window int) (int64, error) {
	total, lines, err := fileCost(path)
	if err != nil {
		return 0, err
	}
	if lines <= window || lines == 0 {
		return total, nil // file is shorter than the window: same as a full read
	}
	return int64(float64(total) * float64(window) / float64(lines)), nil
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p * float64(len(sorted)-1))
	return sorted[i]
}

// ExtRow is the per-extension breakout that answers F4 (does the win
// generalize, or does it concentrate in one language?).
type ExtRow struct {
	Constructs int     `json:"constructs"`
	Files      int     `json:"files"`
	ArmAAvg    int64   `json:"arm_a_avg"`
	ArmBAvg    int64   `json:"arm_b_avg"`
	Ratio      float64 `json:"ratio"`
}

func main() {
	dbPath := flag.String("db", "", "path to a mache-built .db (required)")
	out := flag.String("out", "", "write JSON here instead of stdout")
	readShare := flag.Float64("read-share", 0, "retrieval share of carry-weighted context cost (0..1), measured from this corpus's transcripts; enables the ceiling derivation")
	flag.Parse()
	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "token-bench: -db is required")
		os.Exit(2)
	}
	res, err := measureCorpus(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "token-bench: %v\n", err)
		os.Exit(1)
	}
	if *readShare > 0 {
		// Include this corpus's own measured ratio among the sample points so
		// the artifact shows where this repo actually sits on the curve.
		c := computeCeiling(*readShare, []float64{2, 5, 10, res.RatioMed, 50, 100})
		res.Ceiling = &c
	}
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "token-bench: %v\n", err)
		os.Exit(1)
	}
	b = append(b, '\n')
	if *out != "" {
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "token-bench: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "token-bench: wrote %s\n", *out)
		return
	}
	_, _ = os.Stdout.Write(b)
}

// perFile accumulates arm-B bytes per source file so arm A (the file's own
// size on disk) is counted once regardless of how many constructs it holds.
type perFile struct {
	constructs int
	armB       int64
	sizes      []int64 // per-construct bytes, for paired ratios
	lines      []int   // per-construct line spans, for containment
}

// windowLines matches pgr's baseline read_code default (max_lines=80).
const windowLines = 80

// collectConstructs reads construct-granular rows and groups them by source
// file. Split out of measureCorpus so neither half is a god function.
func collectConstructs(dbPath string) (map[string]*perFile, int, int, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, 0, 0, err
	}
	defer func() { _ = db.Close() }()

	// Construct-granular rows: a function/method's own `source` leaf is exactly
	// what find_definition returns. `record` holds the rendered content.
	rows, err := db.Query(`
		SELECT source_file, LENGTH(record), SUBSTR(record, 1, 200),
		       LENGTH(record) - LENGTH(REPLACE(record, CHAR(10), '')) + 1
		  FROM nodes
		 WHERE name = 'source'
		   AND record IS NOT NULL
		   AND source_file IS NOT NULL
		   AND (id LIKE '%/functions/%' OR id LIKE '%/methods/%')`)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("query constructs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	files := map[string]*perFile{}
	total := 0
	jsonRecords := 0
	for rows.Next() {
		var src string
		var n int64
		var head string
		var nlines int
		if err := rows.Scan(&src, &n, &head, &nlines); err != nil {
			return nil, 0, 0, err
		}
		// `record` is only the rendered content when the schema's
		// content_template is a bare capture (go-schema uses "{{.scope}}").
		// A JSON-object record means the template renders something else and
		// LENGTH(record) is measuring the wrong thing — detect rather than
		// silently mismeasure. Tracked at mache-fc737b.
		if strings.HasPrefix(strings.TrimSpace(head), "{") && json.Valid([]byte(head)) {
			jsonRecords++
		}
		f := files[src]
		if f == nil {
			f = &perFile{}
			files[src] = f
		}
		f.constructs++
		f.armB += n
		f.sizes = append(f.sizes, n)
		f.lines = append(f.lines, nlines)
		total++
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, err
	}
	if total == 0 {
		return nil, 0, 0, fmt.Errorf("no function/method constructs found in %s — was it built with a source schema?", dbPath)
	}
	return files, total, jsonRecords, nil
}

func measureCorpus(dbPath string) (*BenchResult, error) {
	files, total, jsonRecords, err := collectConstructs(dbPath)
	if err != nil {
		return nil, err
	}

	res := &BenchResult{
		DB:         dbPath,
		Constructs: total,
		PerExt:     map[string]ExtRow{},
		Caveats: []string{
			"F1 (escalation) and F2 (completion rate) are NOT measured here — both need a live agent loop. A ratio without them is not evidence of a win.",
			"Arm A charges the whole file once per file. An agent that reads the same file repeatedly pays it again; transcript analysis put 44.1% of Read bytes in multi-read files, so this understates arm A's real cost.",
			"Breakeven is constructs-per-file: a question needing more than that many constructs from one file is cheaper as a whole-file read.",
		},
	}

	type acc struct {
		constructs int
		files      int
		armA, armB int64
	}
	byExt := map[string]*acc{}

	var paired, pairedW, pairedBW []float64
	var constructLines, fileLineCounts []int
	for src, f := range files {
		size, err := armACost(src)
		if err != nil {
			// File moved or deleted since the db was built — skip rather than
			// guess a size, and keep it out of both arms so the ratio stays honest.
			res.Constructs -= f.constructs
			continue
		}
		if f.constructs > res.MaxPerFile {
			res.MaxPerFile = f.constructs
		}
		_, flines, ferr := fileCost(src)
		if ferr == nil {
			fileLineCounts = append(fileLineCounts, flines)
		}
		constructLines = append(constructLines, f.lines...)
		wsize, werr := armWCost(src, windowLines)
		if werr != nil {
			wsize = size
		}
		res.ArmWBytes += wsize
		for _, n := range f.sizes {
			if n > 0 {
				paired = append(paired, float64(size)/float64(n))
				pairedBW = append(pairedBW, float64(wsize)/float64(n))
			}
		}
		if wsize > 0 {
			pairedW = append(pairedW, float64(size)/float64(wsize))
		}
		res.Files++
		res.ArmABytes += size
		res.ArmBBytes += f.armB

		ext := filepath.Ext(src)
		a := byExt[ext]
		if a == nil {
			a = &acc{}
			byExt[ext] = a
		}
		a.constructs += f.constructs
		a.files++
		a.armA += size
		a.armB += f.armB
	}
	if res.Files == 0 || res.Constructs == 0 {
		return nil, fmt.Errorf("no readable source files behind the constructs in %s", dbPath)
	}

	res.ArmAAvg = res.ArmABytes / int64(res.Files)
	res.ArmBAvg = res.ArmBBytes / int64(res.Constructs)
	if res.ArmBAvg > 0 {
		res.Ratio = float64(res.ArmAAvg) / float64(res.ArmBAvg)
	}
	res.Breakeven = float64(res.Constructs) / float64(res.Files)

	// Paired ratios: each construct against ITS OWN file. With constructs/file
	// spanning 1..156, ratio-of-means and mean-of-ratios diverge sharply
	// (Jensen). Median is the headline because it is robust to the files that
	// hold a hundred-plus tiny constructs.
	if len(fileLineCounts) > 0 {
		sort.Ints(fileLineCounts)
		res.MedianFileLines = fileLineCounts[len(fileLineCounts)/2]
	}
	res.Containment = sweepContainment([]int{20, 40, 80, 160, 320}, constructLines, res.MedianFileLines)
	res.WindowLines = windowLines
	if res.Files > 0 {
		res.ArmWAvg = res.ArmWBytes / int64(res.Files)
	}
	if len(pairedW) > 0 {
		sort.Float64s(pairedW)
		res.RatioWMed = percentile(pairedW, 0.5)
	}
	if len(pairedBW) > 0 {
		sort.Float64s(pairedBW)
		res.RatioBWMed = percentile(pairedBW, 0.5)
	}
	if len(paired) > 0 {
		sort.Float64s(paired)
		res.RatioMed = percentile(paired, 0.5)
		res.RatioP25 = percentile(paired, 0.25)
		res.RatioP75 = percentile(paired, 0.75)
		var sum float64
		for _, r := range paired {
			sum += r
		}
		res.RatioMean = sum / float64(len(paired))
	}
	if jsonRecords > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"%d/%d records look like JSON objects — this schema's content_template likely renders something other than the raw record, so arm B is NOT the rendered size. See mache-fc737b.",
			jsonRecords, total))
	}

	for ext, a := range byExt {
		row := ExtRow{Constructs: a.constructs, Files: a.files}
		if a.files > 0 {
			row.ArmAAvg = a.armA / int64(a.files)
		}
		if a.constructs > 0 {
			row.ArmBAvg = a.armB / int64(a.constructs)
		}
		if row.ArmBAvg > 0 {
			row.Ratio = float64(row.ArmAAvg) / float64(row.ArmBAvg)
		}
		res.PerExt[ext] = row
	}

	// Deterministic caveat order so a committed baseline doesn't churn.
	sort.Strings(res.Caveats)
	return res, nil
}
