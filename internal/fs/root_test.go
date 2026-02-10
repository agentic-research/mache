package fs

import (
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/winfsp/cgofuse/fuse"
)

func TestMacheFS_Open(t *testing.T) {
	schema := &api.Topology{Version: "v1alpha1"}
	fs := NewMacheFS(schema)

	tests := []struct {
		name     string
		path     string
		wantErr  int
		wantFh   uint64
	}{
		{
			name:    "open existing file",
			path:    "/hello",
			wantErr: 0,
			wantFh:  0,
		},
		{
			name:    "open non-existent file",
			path:    "/does-not-exist",
			wantErr: fuse.ENOENT,
			wantFh:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errCode, fh := fs.Open(tt.path, 0)
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
	schema := &api.Topology{Version: "v1alpha1"}
	fs := NewMacheFS(schema)

	tests := []struct {
		name     string
		path     string
		wantErr  int
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
			name:    "stat hello file",
			path:    "/hello",
			wantErr: 0,
			checkStat: func(t *testing.T, stat *fuse.Stat_t) {
				if stat.Mode&fuse.S_IFREG == 0 {
					t.Error("Hello should be a regular file")
				}
				expectedSize := int64(len("Hello, World!\n"))
				if stat.Size != expectedSize {
					t.Errorf("Hello size = %v, want %v", stat.Size, expectedSize)
				}
			},
		},
		{
			name:    "stat non-existent path",
			path:    "/does-not-exist",
			wantErr: fuse.ENOENT,
			checkStat: func(t *testing.T, stat *fuse.Stat_t) {
				// No stat to check on error
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stat fuse.Stat_t
			errCode := fs.Getattr(tt.path, &stat, 0)
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
	schema := &api.Topology{Version: "v1alpha1"}
	fs := NewMacheFS(schema)

	tests := []struct {
		name    string
		path    string
		wantErr int
		wantEntries []string
	}{
		{
			name:    "readdir root",
			path:    "/",
			wantErr: 0,
			wantEntries: []string{".", "..", "hello"},
		},
		{
			name:    "readdir non-existent directory",
			path:    "/does-not-exist",
			wantErr: fuse.ENOENT,
			wantEntries: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var entries []string
			fill := func(name string, stat *fuse.Stat_t, ofst int64) bool {
				entries = append(entries, name)
				return true
			}

			errCode := fs.Readdir(tt.path, fill, 0, 0)
			if errCode != tt.wantErr {
				t.Errorf("Readdir() errCode = %v, want %v", errCode, tt.wantErr)
			}

			if errCode == 0 {
				if len(entries) != len(tt.wantEntries) {
					t.Errorf("Readdir() got %v entries, want %v", len(entries), len(tt.wantEntries))
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
	schema := &api.Topology{Version: "v1alpha1"}
	fs := NewMacheFS(schema)

	tests := []struct {
		name     string
		path     string
		offset   int64
		buffSize int
		wantN    int
		wantData string
	}{
		{
			name:     "read hello file from start",
			path:     "/hello",
			offset:   0,
			buffSize: 100,
			wantN:    14,
			wantData: "Hello, World!\n",
		},
		{
			name:     "read hello file with offset",
			path:     "/hello",
			offset:   7,
			buffSize: 100,
			wantN:    7,
			wantData: "World!\n",
		},
		{
			name:     "read past end of file",
			path:     "/hello",
			offset:   100,
			buffSize: 100,
			wantN:    0,
			wantData: "",
		},
		{
			name:     "read non-existent file",
			path:     "/does-not-exist",
			offset:   0,
			buffSize: 100,
			wantN:    fuse.ENOENT,
			wantData: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buff := make([]byte, tt.buffSize)
			n := fs.Read(tt.path, buff, tt.offset, 0)

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
	// Verify that cgofuse error constants are positive
	// cgofuse returns positive errno values (unlike some other FUSE libraries)
	// These should be returned directly without negation
	if fuse.ENOENT <= 0 {
		t.Errorf("fuse.ENOENT = %v, expected positive value", fuse.ENOENT)
	}
}
