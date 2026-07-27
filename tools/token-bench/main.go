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
type Result struct {
	DB         string            `json:"db"`
	Constructs int               `json:"constructs"`
	Files      int               `json:"files"`
	ArmABytes  int64             `json:"arm_a_bytes"`  // whole-file retrieval, incl. cat -n prefixes
	ArmBBytes  int64             `json:"arm_b_bytes"`  // construct-granular retrieval
	ArmAAvg    int64             `json:"arm_a_avg"`    // bytes per file
	ArmBAvg    int64             `json:"arm_b_avg"`    // bytes per construct
	Ratio      float64           `json:"ratio"`        // ratio-of-means: arm_a_avg / arm_b_avg
	RatioMed   float64           `json:"ratio_median"` // median PAIRED ratio — the headline statistic
	RatioMean  float64           `json:"ratio_mean"`   // mean paired ratio (outlier-sensitive)
	RatioP25   float64           `json:"ratio_p25"`
	RatioP75   float64           `json:"ratio_p75"`
	Breakeven  float64           `json:"breakeven"` // constructs/file — above this, arm A wins
	MaxPerFile int               `json:"max_constructs_per_file"`
	PerExt     map[string]ExtRow `json:"per_ext"` // F4
	Caveats    []string          `json:"caveats"`
	Warnings   []string          `json:"warnings,omitempty"`
}

// armACost is what the built-in Read tool actually puts in context: the file
// bytes PLUS cat -n line-number prefixes ("%d\t" per line). Charging raw file
// size understates arm A by ~18% on typical source, which understates the
// win — measured, not assumed.
func armACost(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	lines := bytes.Count(b, []byte{'\n'})
	if len(b) > 0 && b[len(b)-1] != '\n' {
		lines++ // trailing line without a newline still gets numbered
	}
	overhead := 0
	for i := 1; i <= lines; i++ {
		overhead += len(strconv.Itoa(i)) + 1 // number + tab
	}
	return int64(len(b) + overhead), nil
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
	flag.Parse()
	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "token-bench: -db is required")
		os.Exit(2)
	}
	res, err := run(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "token-bench: %v\n", err)
		os.Exit(1)
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
}

func run(dbPath string) (*Result, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	// Construct-granular rows: a function/method's own `source` leaf is exactly
	// what find_definition returns. `record` holds the rendered content.
	rows, err := db.Query(`
		SELECT source_file, LENGTH(record), SUBSTR(record, 1, 200)
		  FROM nodes
		 WHERE name = 'source'
		   AND record IS NOT NULL
		   AND source_file IS NOT NULL
		   AND (id LIKE '%/functions/%' OR id LIKE '%/methods/%')`)
	if err != nil {
		return nil, fmt.Errorf("query constructs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	files := map[string]*perFile{}
	total := 0
	jsonRecords := 0
	for rows.Next() {
		var src string
		var n int64
		var head string
		if err := rows.Scan(&src, &n, &head); err != nil {
			return nil, err
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
		total++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if total == 0 {
		return nil, fmt.Errorf("no function/method constructs found in %s — was it built with a source schema?", dbPath)
	}

	res := &Result{
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

	var paired []float64
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
		for _, n := range f.sizes {
			if n > 0 {
				paired = append(paired, float64(size)/float64(n))
			}
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
