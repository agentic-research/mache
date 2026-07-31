package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/internal/leyline"
)

// TestRelForEnrich pins how mache maps an MCP `file` param to the
// source-relative path the daemon `enrich` op expects (mache-303036).
func TestRelForEnrich(t *testing.T) {
	leyline.SetDaemonSource("/src/root")
	t.Cleanup(func() { leyline.SetDaemonSource("") })

	cases := []struct{ in, want string }{
		{"/src/root/crates/x/lib.rs", "crates/x/lib.rs"}, // absolute under root → relativized
		{"crates/x/lib.rs", "crates/x/lib.rs"},           // already relative → unchanged
		{"/elsewhere/y.rs", "/elsewhere/y.rs"},           // outside root → passthrough
	}
	for _, c := range cases {
		if got := relForEnrich(c.in); got != c.want {
			t.Errorf("relForEnrich(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// With no source configured, paths pass through untouched.
	leyline.SetDaemonSource("")
	if got := relForEnrich("/src/root/a.rs"); got != "/src/root/a.rs" {
		t.Errorf("no-source passthrough: got %q", got)
	}
}

// TestServedSourceDir confirms a directory arg yields a source root while a
// .db arg yields "" (no live enrichment for pre-baked dbs).
func TestServedSourceDir(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "code.db")
	if err := os.WriteFile(db, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := servedSourceDir([]string{dir}, "", false); got != dir {
		t.Errorf("dir arg: got %q want %q", got, dir)
	}
	if got := servedSourceDir([]string{db}, "", false); got != "" {
		t.Errorf(".db arg should yield empty source, got %q", got)
	}
	if got := servedSourceDir(nil, dir, true); got != dir {
		t.Errorf("explicit base source: got %q want %q", got, dir)
	}
	if got := servedSourceDir(nil, dir, false); got != "" {
		t.Errorf("implicit base path should be empty, got %q", got)
	}
	if got := servedSourceDir([]string{db}, dir, true); got != "" {
		t.Errorf("positional .db must take precedence over explicit base path, got %q", got)
	}
}

func TestConfigureManagedLeylineRuntime_CDCRequiresManagedSource(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "code.db")
	if err := os.WriteFile(db, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		leyline.SetDaemonSource("")
		leyline.SetDaemonCDC(false)
	})

	for _, tc := range []struct {
		name           string
		args           []string
		basePath       string
		baseIsSource   bool
		externalDaemon bool
		wantSource     string
		wantCDC        bool
	}{
		{
			name: "positional source", args: []string{dir},
			wantSource: dir, wantCDC: true,
		},
		{
			name: "explicit path source", basePath: dir, baseIsSource: true,
			wantSource: dir, wantCDC: true,
		},
		{
			name: "positional prebuilt database", args: []string{db},
			basePath: dir, baseIsSource: true,
		},
		{
			name: "external control daemon", args: []string{dir}, externalDaemon: true,
			wantSource: dir,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leyline.SetDaemonSource("stale-source")
			leyline.SetDaemonCDC(true)

			configureManagedLeylineRuntime(tc.args, tc.basePath, tc.baseIsSource, tc.externalDaemon, true)

			if got := leyline.DaemonSource(); got != tc.wantSource {
				t.Errorf("DaemonSource() = %q, want %q", got, tc.wantSource)
			}
			if got := leyline.DaemonCDC(); got != tc.wantCDC {
				t.Errorf("DaemonCDC() = %t, want %t", got, tc.wantCDC)
			}
		})
	}
}
