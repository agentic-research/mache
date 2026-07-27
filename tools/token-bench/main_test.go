package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// fixture builds a minimal nodes table plus real files on disk, so the arm-A
// side (which stats the filesystem) has something to measure.
func fixture(t *testing.T, files map[string]string, constructs [][2]any) string {
	t.Helper()
	dir := t.TempDir()
	abs := map[string]string{}
	for name, body := range files {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
		abs[name] = p
	}
	dbPath := filepath.Join(dir, "t.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	_, err = db.Exec(`CREATE TABLE nodes (id TEXT PRIMARY KEY, name TEXT, record TEXT, source_file TEXT)`)
	require.NoError(t, err)
	for i, c := range constructs {
		file := c[0].(string)
		body := c[1].(string)
		id := "pkg/functions/F" + string(rune('a'+i)) + "/source"
		_, err = db.Exec(`INSERT INTO nodes (id,name,record,source_file) VALUES (?,'source',?,?)`,
			id, body, abs[file])
		require.NoError(t, err)
	}
	return dbPath
}

// The core arithmetic: arm A is charged once per FILE, arm B once per
// CONSTRUCT. Getting this backwards (charging arm A per construct) would
// inflate the ratio by exactly the constructs-per-file factor — the single
// most likely way this bench could flatter mache.
func TestRun_ArmAChargedPerFileNotPerConstruct(t *testing.T) {
	body := "0123456789" // 10 bytes
	dbPath := fixture(t,
		map[string]string{"a.go": "0123456789012345678901234567890123456789"}, // 40 bytes
		[][2]any{{"a.go", body}, {"a.go", body}, {"a.go", body}, {"a.go", body}},
	)
	res, err := run(dbPath)
	require.NoError(t, err)

	assert.Equal(t, 4, res.Constructs)
	assert.Equal(t, 1, res.Files, "one file backs all four constructs")
	assert.EqualValues(t, 42, res.ArmABytes, "arm A charges the file once (40B + one 2B cat -n prefix), not four times")
	assert.EqualValues(t, 40, res.ArmBBytes, "four constructs x 10 bytes")
	assert.EqualValues(t, 42, res.ArmAAvg)
	assert.EqualValues(t, 10, res.ArmBAvg)
	assert.InDelta(t, 4.2, res.Ratio, 0.001)
	assert.InDelta(t, 4.0, res.Breakeven, 0.001, "4 constructs / 1 file")
}

// F4: the per-extension breakout must actually separate languages, or a win
// concentrated in one language would read as a general win.
func TestRun_PerExtensionSeparatesLanguages(t *testing.T) {
	dbPath := fixture(t,
		map[string]string{
			"a.go": "aaaaaaaaaaaaaaaaaaaa", // 20
			"b.rs": "bbbbbbbbbb",           // 10
		},
		[][2]any{{"a.go", "aa"}, {"b.rs", "bbbbb"}},
	)
	res, err := run(dbPath)
	require.NoError(t, err)

	require.Contains(t, res.PerExt, ".go")
	require.Contains(t, res.PerExt, ".rs")
	assert.InDelta(t, 11.0, res.PerExt[".go"].Ratio, 0.001, "(20B + 2B prefix) / 2-byte construct")
	assert.InDelta(t, 2.4, res.PerExt[".rs"].Ratio, 0.001, "(10B + 2B prefix) / 5-byte construct")
}

// A construct whose source file no longer exists must be dropped from BOTH
// arms. Counting it in arm B while skipping it in arm A would silently shrink
// arm_b_avg and overstate the ratio.
func TestRun_MissingSourceFileDroppedFromBothArms(t *testing.T) {
	dbPath := fixture(t,
		map[string]string{"a.go": "aaaaaaaaaa"},
		[][2]any{{"a.go", "aa"}},
	)
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO nodes (id,name,record,source_file) VALUES
		('pkg/functions/Gone/source','source','xxxxxxxxxxxxxxxxxxxx','/nonexistent/gone.go')`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	res, err := run(dbPath)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Constructs, "the vanished construct is not counted")
	assert.Equal(t, 1, res.Files)
	assert.EqualValues(t, 2, res.ArmBBytes, "its bytes do not leak into arm B")
}

// An empty or non-source db must fail loudly rather than emit a zero ratio
// that a reader would mistake for "no benefit".
func TestRun_NoConstructsIsAnError(t *testing.T) {
	dbPath := fixture(t, map[string]string{"a.go": "a"}, nil)
	_, err := run(dbPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no function/method constructs")
}

// The caveats are load-bearing: they are what stops a ratio being read as a
// proven win. Assert they ship and name the unmeasured falsifiers.
func TestRun_CaveatsNameUnmeasuredFalsifiers(t *testing.T) {
	dbPath := fixture(t, map[string]string{"a.go": "aaaa"}, [][2]any{{"a.go", "a"}})
	res, err := run(dbPath)
	require.NoError(t, err)
	joined := ""
	for _, c := range res.Caveats {
		joined += c + "\n"
	}
	assert.Contains(t, joined, "F1")
	assert.Contains(t, joined, "F2")
}

// Arm A must charge what Read actually emits: file bytes PLUS cat -n line
// prefixes. Charging raw file size understates arm A ~18% on real source,
// which understates the win — the wrong direction to be wrong in, but wrong.
func TestArmACost_ChargesLineNumberPrefixes(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.go")
	// 3 lines, 6 content bytes ("a\nb\nc\n"); prefixes are "1\t","2\t","3\t" = 6
	require.NoError(t, os.WriteFile(p, []byte("a\nb\nc\n"), 0o644))
	got, err := armACost(p)
	require.NoError(t, err)
	assert.EqualValues(t, 12, got, "6 content bytes + 6 prefix bytes")
}

// A trailing line with no newline is still numbered by cat -n.
func TestArmACost_TrailingLineWithoutNewline(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.go")
	require.NoError(t, os.WriteFile(p, []byte("a\nb"), 0o644))
	got, err := armACost(p)
	require.NoError(t, err)
	assert.EqualValues(t, 7, got, "3 content bytes + 2 lines x 2 prefix bytes")
}

// The headline is the MEDIAN paired ratio, not the mean. With constructs/file
// spanning 1..156 in the real corpus, the mean is dragged by files holding a
// hundred tiny constructs; reporting it as the headline would overstate the
// win by ~2.7x. Pin that median and mean are computed distinctly.
func TestRun_PairedRatioMedianIsRobustToOutlierFile(t *testing.T) {
	// Mirror the real corpus shape: RIGHT-skewed. Many ordinary constructs each
	// in their own modest file (low ratio), plus one huge file crammed with tiny
	// constructs (very high ratio). Measured corpus: median 26x, mean 69x.
	files := map[string]string{"huge.go": string(make([]byte, 2000))}
	cons := [][2]any{}
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("s%02d.go", i)
		files[name] = "0123456789012345678901234567890123456789"  // 40B -> 42 w/ prefix
		cons = append(cons, [2]any{name, "01234567890123456789"}) // 20B -> ratio 2.1
	}
	for i := 0; i < 3; i++ {
		cons = append(cons, [2]any{"huge.go", "x"}) // 2002/1 -> ratio ~2002
	}
	dbPath := fixture(t, files, cons)
	res, err := run(dbPath)
	require.NoError(t, err)

	assert.Equal(t, 3, res.MaxPerFile, "the crammed file is identified")
	assert.InDelta(t, 2.1, res.RatioMed, 0.2, "median tracks the typical construct")
	assert.Greater(t, res.RatioMean, res.RatioMed*10,
		"mean is dragged far above the median by the outlier file — which is why the median is the headline")
}

// A JSON-object record means the template renders something other than the
// raw record, so LENGTH(record) is the wrong measure — must WARN, not
// silently report an inflated ratio. See mache-fc737b.
func TestRun_WarnsWhenRecordLooksLikeJSON(t *testing.T) {
	dbPath := fixture(t,
		map[string]string{"a.go": "aaaaaaaaaa"},
		[][2]any{{"a.go", `{"scope":"func f(){}","extra":1}`}},
	)
	res, err := run(dbPath)
	require.NoError(t, err)
	require.NotEmpty(t, res.Warnings, "must warn rather than silently mismeasure")
	assert.Contains(t, res.Warnings[0], "mache-fc737b")
}
