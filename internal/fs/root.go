package fs

import (
	"time"

	"github.com/agentic-research/mache/api"
	"github.com/winfsp/cgofuse/fuse"
)

// MacheFS implements the FUSE interface from cgofuse
type MacheFS struct {
	fuse.FileSystemBase
	Schema    *api.Topology
	mountTime fuse.Timespec
}

func NewMacheFS(schema *api.Topology) *MacheFS {
	return &MacheFS{
		Schema:    schema,
		mountTime: fuse.NewTimespec(time.Now()),
	}
}

// Open (Lookup + Open combined in simplistic FS)
// For a Hello World, we just check the path.
func (fs *MacheFS) Open(path string, flags int) (int, uint64) {
	if path == "/hello" {
		return 0, 0 // Success (0)
	}
	return -fuse.ENOENT, 0
}

// Getattr (Stat)
func (fs *MacheFS) Getattr(path string, stat *fuse.Stat_t, fh uint64) int {
	switch path {
	case "/":
		stat.Mode = fuse.S_IFDIR | 0o555
		stat.Nlink = 2
		stat.Atim = fs.mountTime
		stat.Mtim = fs.mountTime
		stat.Ctim = fs.mountTime
		stat.Birthtim = fs.mountTime
		return 0
	case "/hello":
		stat.Mode = fuse.S_IFREG | 0o444
		stat.Nlink = 1
		stat.Size = int64(len("Hello, World!\n"))
		stat.Atim = fs.mountTime
		stat.Mtim = fs.mountTime
		stat.Ctim = fs.mountTime
		stat.Birthtim = fs.mountTime
		return 0
	default:
		return -fuse.ENOENT
	}
}

// Readdir (List directory)
func (fs *MacheFS) Readdir(path string, fill func(name string, stat *fuse.Stat_t, ofst int64) bool, ofst int64, fh uint64) int {
	if path == "/" {
		fill(".", nil, 0)
		fill("..", nil, 0)
		fill("hello", nil, 0)
		return 0
	}
	return -fuse.ENOENT
}

// Read (Cat file)
func (fs *MacheFS) Read(path string, buff []byte, ofst int64, fh uint64) int {
	if path == "/hello" {
		content := []byte("Hello, World!\n")
		if ofst >= int64(len(content)) {
			return 0
		}
		end := ofst + int64(len(buff))
		if end > int64(len(content)) {
			end = int64(len(content))
		}
		n := copy(buff, content[ofst:end])
		return n
	}
	return -fuse.ENOENT
}
