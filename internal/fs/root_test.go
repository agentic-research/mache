package fs

import (
	"encoding/json"
	"io/fs"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
	"github.com/winfsp/cgofuse/fuse"
)

// newTestFS creates a MacheFS with a pre-populated MemoryStore for testing.
// Every node has an explicit Mode — no heuristics.
func newTestFS() *MacheFS {
	schema := &api.Topology{Version: "v1alpha1"}
	store := graph.NewMemoryStore()

	store.AddRoot(&graph.Node{
		ID:   "vulns",
		Mode: fs.ModeDir,
		Children: []string{
			"vulns/CVE-2024-1234",
			"vulns/CVE-2024-5678",
		},
	})
	store.AddNode(&graph.Node{
		ID:   "vulns/CVE-2024-1234",
		Mode: fs.ModeDir,
		Children: []string{
			"vulns/CVE-2024-1234/description",
			"vulns/CVE-2024-1234/severity",
		},
	})
	store.AddNode(&graph.Node{
		ID:   "vulns/CVE-2024-1234/description",
		Mode: 0, // regular file
		Data: []byte("Buffer overflow in example.c\n"),
	})
	store.AddNode(&graph.Node{
		ID:   "vulns/CVE-2024-1234/severity",
		Mode: 0, // regular file
		Data: []byte("CRITICAL\n"),
	})
	store.AddNode(&graph.Node{
		ID:   "vulns/CVE-2024-5678",
		Mode: fs.ModeDir,
		Children: []string{
			"vulns/CVE-2024-5678/description",
			"vulns/CVE-2024-5678/severity",
		},
	})
	store.AddNode(&graph.Node{
		ID:   "vulns/CVE-2024-5678/description",
		Mode: 0,
		Data: []byte("Null pointer dereference\n"),
	})
	store.AddNode(&graph.Node{
		ID:   "vulns/CVE-2024-5678/severity",
		Mode: 0,
		Data: []byte("LOW\n"),
	})

	return NewMacheFS(schema, store)
}

func TestMacheFS_Open(t *testing.T) {
	mfs := newTestFS()

	tests := []struct {
		name    string
		path    string
		wantErr int
		wantFh  uint64
	}{
		{
			name:    "open existing file node",
			path:    "/vulns/CVE-2024-1234/severity",
			wantErr: 0,
			wantFh:  0,
		},
		{
			name:    "open non-existent path",
			path:    "/does-not-exist",
			wantErr: -fuse.ENOENT,
			wantFh:  0,
		},
		{
			name:    "open directory returns EISDIR",
			path:    "/vulns",
			wantErr: -fuse.EISDIR,
			wantFh:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errCode, fh := mfs.Open(tt.path, 0)
			if errCode != tt.wantErr {
				t.Errorf("Open() errCode = %v, want %v", errCode, tt.wantErr)
			}
			if fh != tt.wantFh {
				t.Errorf("Open() fh = %v, want %v", fh, tt.wantFh)
			}
		})
	}
}

func TestMacheFS_Getattr(t *testing.T) {
	mfs := newTestFS()

	tests := []struct {
		name      string
		path      string
		wantErr   int
		checkStat func(*testing.T, *fuse.Stat_t)
	}{
		{
			name:    "stat root directory",
			path:    "/",
			wantErr: 0,
			checkStat: func(t *testing.T, stat *fuse.Stat_t) {
				if stat.Mode&fuse.S_IFDIR == 0 {
					t.Error("Root should be a directory")
				}
				if stat.Nlink != 2 {
					t.Errorf("Root nlink = %v, want 2", stat.Nlink)
				}
			},
		},
		{
			name:    "stat directory node",
			path:    "/vulns",
			wantErr: 0,
			checkStat: func(t *testing.T, stat *fuse.Stat_t) {
				if stat.Mode&fuse.S_IFDIR == 0 {
					t.Error("vulns should be a directory")
				}
			},
		},
		{
			name:    "stat nested directory node",
			path:    "/vulns/CVE-2024-1234",
			wantErr: 0,
			checkStat: func(t *testing.T, stat *fuse.Stat_t) {
				if stat.Mode&fuse.S_IFDIR == 0 {
					t.Error("CVE node should be a directory")
				}
			},
		},
		{
			name:    "stat file node",
			path:    "/vulns/CVE-2024-1234/severity",
			wantErr: 0,
			checkStat: func(t *testing.T, stat *fuse.Stat_t) {
				if stat.Mode&fuse.S_IFREG == 0 {
					t.Error("severity should be a regular file")
				}
				expectedSize := int64(len("CRITICAL\n"))
				if stat.Size != expectedSize {
					t.Errorf("severity size = %v, want %v", stat.Size, expectedSize)
				}
			},
		},
		{
			name:    "stat non-existent path",
			path:    "/does-not-exist",
			wantErr: -fuse.ENOENT,
			checkStat: func(t *testing.T, stat *fuse.Stat_t) {
				// No stat to check on error
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stat fuse.Stat_t
			errCode := mfs.Getattr(tt.path, &stat, 0)
			if errCode != tt.wantErr {
				t.Errorf("Getattr() errCode = %v, want %v", errCode, tt.wantErr)
			}
			if errCode == 0 && tt.checkStat != nil {
				tt.checkStat(t, &stat)
			}
		})
	}
}

func TestMacheFS_Readdir(t *testing.T) {
	mfs := newTestFS()

	tests := []struct {
		name        string
		path        string
		wantErr     int
		wantEntries []string
	}{
		{
			name:        "readdir root lists graph roots",
			path:        "/",
			wantErr:     0,
			wantEntries: []string{".", "..", "_schema.json", "vulns"},
		},
		{
			name:        "readdir vulns lists CVEs",
			path:        "/vulns",
			wantErr:     0,
			wantEntries: []string{".", "..", "CVE-2024-1234", "CVE-2024-5678"},
		},
		{
			name:        "readdir CVE lists file children",
			path:        "/vulns/CVE-2024-1234",
			wantErr:     0,
			wantEntries: []string{".", "..", "description", "severity"},
		},
		{
			name:    "readdir non-existent path",
			path:    "/does-not-exist",
			wantErr: -fuse.ENOENT,
		},
		// Note: "readdir on a file" is now caught by Opendir (ENOTDIR).
		// See TestMacheFS_Opendir_Errors.
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var entries []string
			fill := func(name string, stat *fuse.Stat_t, ofst int64) bool {
				entries = append(entries, name)
				return true // true = accepted, keep sending
			}

			errCode := mfs.Readdir(tt.path, fill, 0, 0)
			if errCode != tt.wantErr {
				t.Errorf("Readdir() errCode = %v, want %v", errCode, tt.wantErr)
			}

			if errCode == 0 && tt.wantEntries != nil {
				if len(entries) != len(tt.wantEntries) {
					t.Errorf("Readdir() got %v entries %v, want %v entries %v", len(entries), entries, len(tt.wantEntries), tt.wantEntries)
				}
				for i, want := range tt.wantEntries {
					if i >= len(entries) || entries[i] != want {
						t.Errorf("Readdir() entry[%d] = %v, want %v", i, entries[i], want)
					}
				}
			}
		})
	}
}

func TestMacheFS_Readdir_AllEntriesReturned(t *testing.T) {
	mfs := newTestFS()

	var entries []string
	fill := func(name string, stat *fuse.Stat_t, ofst int64) bool {
		entries = append(entries, name)
		return true // true = accepted, keep sending
	}

	errCode := mfs.Readdir("/vulns", fill, 0, 0)
	if errCode != 0 {
		t.Fatalf("Readdir() errCode = %v, want 0", errCode)
	}
	want := []string{".", "..", "CVE-2024-1234", "CVE-2024-5678"}
	if len(entries) != len(want) {
		t.Fatalf("got %v entries %v, want %v", len(entries), entries, want)
	}
	for i, w := range want {
		if entries[i] != w {
			t.Errorf("entry[%d] = %q, want %q", i, entries[i], w)
		}
	}
}

func TestMacheFS_Readdir_BufferFull(t *testing.T) {
	mfs := newTestFS()

	// fill returning false means buffer full — we stop after the first entry.
	var entries []string
	fill := func(name string, stat *fuse.Stat_t, ofst int64) bool {
		entries = append(entries, name)
		return false // false = buffer full, stop sending
	}

	errCode := mfs.Readdir("/vulns", fill, 0, 0)
	if errCode != 0 {
		t.Fatalf("Readdir errCode = %v, want 0", errCode)
	}
	// Buffer full on first fill → only "." is returned
	if len(entries) != 1 || entries[0] != "." {
		t.Fatalf("entries = %v, want [\".\"]", entries)
	}
}

// Regression test: the fill() convention was inverted, causing auto-mode readdir
// to return only "." and ".." (fill returned true=accepted, old code broke on true).
// This test ensures ALL children are returned when fill always accepts (returns true).
func TestMacheFS_Readdir_FillConventionRegression(t *testing.T) {
	mfs := newTestFS()

	var entries []string
	fill := func(name string, stat *fuse.Stat_t, ofst int64) bool {
		entries = append(entries, name)
		return true // cgofuse: true = accepted, continue
	}

	errCode := mfs.Readdir("/vulns/CVE-2024-1234", fill, 0, 0)
	if errCode != 0 {
		t.Fatalf("Readdir errCode = %v, want 0", errCode)
	}

	// Must contain all file children, not just "." and ".."
	if len(entries) < 3 {
		t.Fatalf("fill convention bug: only got %v — expected at least 3 (., .., + children)", entries)
	}
	// Verify specific children are present (not just . and ..)
	found := make(map[string]bool)
	for _, e := range entries {
		found[e] = true
	}
	for _, want := range []string{"description", "severity"} {
		if !found[want] {
			t.Errorf("missing child %q in entries %v", want, entries)
		}
	}
}

func TestMacheFS_Opendir_Errors(t *testing.T) {
	mfs := newTestFS()

	errCode, _ := mfs.Opendir("/does-not-exist")
	if errCode != -fuse.ENOENT {
		t.Errorf("Opendir(nonexistent) = %v, want ENOENT", errCode)
	}

	errCode, _ = mfs.Opendir("/vulns/CVE-2024-1234/severity")
	if errCode != -fuse.ENOTDIR {
		t.Errorf("Opendir(file) = %v, want ENOTDIR", errCode)
	}
}

func TestMacheFS_Opendir_Readdir_Releasedir(t *testing.T) {
	mfs := newTestFS()

	// Opendir caches the entry list
	errCode, fh := mfs.Opendir("/vulns")
	if errCode != 0 {
		t.Fatalf("Opendir errCode = %v, want 0", errCode)
	}

	// Readdir returns all entries via auto-mode (offset=0 to fill)
	var entries []string
	fill := func(name string, stat *fuse.Stat_t, ofst int64) bool {
		entries = append(entries, name)
		return true
	}

	errCode = mfs.Readdir("/vulns", fill, 0, fh)
	if errCode != 0 {
		t.Fatalf("Readdir errCode = %v, want 0", errCode)
	}
	want := []string{".", "..", "CVE-2024-1234", "CVE-2024-5678"}
	if len(entries) != len(want) {
		t.Fatalf("entries = %v, want %v", entries, want)
	}
	for i, w := range want {
		if entries[i] != w {
			t.Errorf("entry[%d] = %q, want %q", i, entries[i], w)
		}
	}

	// Releasedir frees the handle
	errCode = mfs.Releasedir("/vulns", fh)
	if errCode != 0 {
		t.Fatalf("Releasedir errCode = %v, want 0", errCode)
	}

	// Verify handle is released — subsequent Readdir falls back to graph
	var fallback []string
	fill2 := func(name string, stat *fuse.Stat_t, ofst int64) bool {
		fallback = append(fallback, name)
		return true
	}
	errCode = mfs.Readdir("/vulns", fill2, 0, fh)
	if errCode != 0 {
		t.Fatalf("Readdir after release errCode = %v, want 0", errCode)
	}
	if len(fallback) != len(want) {
		t.Fatalf("fallback entries = %v, want %v", fallback, want)
	}
}

func TestMacheFS_Read(t *testing.T) {
	mfs := newTestFS()

	tests := []struct {
		name     string
		path     string
		offset   int64
		buffSize int
		wantN    int
		wantData string
	}{
		{
			name:     "read file node from start",
			path:     "/vulns/CVE-2024-1234/severity",
			offset:   0,
			buffSize: 100,
			wantN:    len("CRITICAL\n"),
			wantData: "CRITICAL\n",
		},
		{
			name:     "read another file node",
			path:     "/vulns/CVE-2024-1234/description",
			offset:   0,
			buffSize: 100,
			wantN:    len("Buffer overflow in example.c\n"),
			wantData: "Buffer overflow in example.c\n",
		},
		{
			name:     "read with offset",
			path:     "/vulns/CVE-2024-1234/severity",
			offset:   4,
			buffSize: 100,
			wantN:    len("ICAL\n"),
			wantData: "ICAL\n",
		},
		{
			name:     "read past end of file",
			path:     "/vulns/CVE-2024-1234/severity",
			offset:   100,
			buffSize: 100,
			wantN:    0,
			wantData: "",
		},
		{
			name:     "read non-existent path",
			path:     "/does-not-exist",
			offset:   0,
			buffSize: 100,
			wantN:    -fuse.ENOENT,
			wantData: "",
		},
		{
			name:     "read a directory returns EISDIR",
			path:     "/vulns/CVE-2024-1234",
			offset:   0,
			buffSize: 100,
			wantN:    -fuse.EISDIR,
			wantData: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buff := make([]byte, tt.buffSize)
			n := mfs.Read(tt.path, buff, tt.offset, 0)

			if n != tt.wantN {
				t.Errorf("Read() n = %v, want %v", n, tt.wantN)
			}

			if n > 0 && tt.wantData != "" {
				got := string(buff[:n])
				if got != tt.wantData {
					t.Errorf("Read() data = %q, want %q", got, tt.wantData)
				}
			}
		})
	}
}

func TestMacheFS_SchemaFile(t *testing.T) {
	mfs := newTestFS()

	// Getattr returns a regular file with correct size
	var stat fuse.Stat_t
	errCode := mfs.Getattr("/_schema.json", &stat, 0)
	if errCode != 0 {
		t.Fatalf("Getattr(/_schema.json) = %v, want 0", errCode)
	}
	if stat.Mode&fuse.S_IFREG == 0 {
		t.Error("_schema.json should be a regular file")
	}
	if stat.Size == 0 {
		t.Error("_schema.json size should be > 0")
	}

	// Open succeeds read-only
	errCode, _ = mfs.Open("/_schema.json", 0)
	if errCode != 0 {
		t.Fatalf("Open(/_schema.json) = %v, want 0", errCode)
	}

	// Read returns valid JSON matching the schema
	buf := make([]byte, stat.Size)
	n := mfs.Read("/_schema.json", buf, 0, 0)
	if int64(n) != stat.Size {
		t.Fatalf("Read() n = %v, want %v", n, stat.Size)
	}

	var parsed api.Topology
	if err := json.Unmarshal(buf[:n], &parsed); err != nil {
		t.Fatalf("_schema.json is not valid JSON: %v", err)
	}
	if parsed.Version != "v1alpha1" {
		t.Errorf("parsed version = %q, want %q", parsed.Version, "v1alpha1")
	}

	// Root readdir includes _schema.json
	errCode2, fh := mfs.Opendir("/")
	if errCode2 != 0 {
		t.Fatalf("Opendir(/) = %v, want 0", errCode2)
	}
	var entries []string
	fill := func(name string, stat *fuse.Stat_t, ofst int64) bool {
		entries = append(entries, name)
		return true
	}
	mfs.Readdir("/", fill, 0, fh)
	found := false
	for _, e := range entries {
		if e == "_schema.json" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("root readdir missing _schema.json, got %v", entries)
	}
}

func TestMacheFS_ErrorCodesArePositive(t *testing.T) {
	if fuse.ENOENT <= 0 {
		t.Errorf("fuse.ENOENT = %v, expected positive value", fuse.ENOENT)
	}
}
