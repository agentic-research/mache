package fs

import (
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
			wantEntries: []string{".", "..", "vulns"},
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
		{
			name:    "readdir on a file node returns ENOTDIR",
			path:    "/vulns/CVE-2024-1234/severity",
			wantErr: -fuse.ENOTDIR,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var entries []string
			fill := func(name string, stat *fuse.Stat_t, ofst int64) bool {
				entries = append(entries, name)
				return true
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

func TestMacheFS_ErrorCodesArePositive(t *testing.T) {
	if fuse.ENOENT <= 0 {
		t.Errorf("fuse.ENOENT = %v, expected positive value", fuse.ENOENT)
	}
}
