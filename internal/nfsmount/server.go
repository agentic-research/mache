package nfsmount

import (
	"fmt"
	"net"
	"os/exec"
	"runtime"

	billy "github.com/go-git/go-billy/v5"
	nfs "github.com/willscott/go-nfs"
	nfshelper "github.com/willscott/go-nfs/helpers"
)

// Server manages the NFS server lifecycle.
type Server struct {
	listener net.Listener
	port     int
}

// NewServer starts an NFS server on an ephemeral port backed by the given filesystem.
func NewServer(fs billy.Filesystem) (*Server, error) {
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return nil, fmt.Errorf("nfs listen: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	handler := nfshelper.NewNullAuthHandler(fs)
	cacheHelper := nfshelper.NewCachingHandler(handler, 4096)

	go func() {
		_ = nfs.Serve(listener, cacheHelper)
	}()

	return &Server{listener: listener, port: port}, nil
}

// Port returns the TCP port the NFS server is listening on.
func (s *Server) Port() int {
	return s.port
}

// Close stops the NFS server by closing the listener.
func (s *Server) Close() error {
	return s.listener.Close()
}

// BuildMountOpts returns the NFS mount options string for the given OS.
func BuildMountOpts(goos string, port int, writable bool, extraOpts string) (string, error) {
	var opts string
	switch goos {
	case "darwin":
		// noac: disable attribute caching so dynamic graph changes (new tabs, schema updates)
		// are visible immediately. Without this, macOS NFS client caches empty dir listings.
		opts = fmt.Sprintf("port=%d,mountport=%d,vers=3,tcp,locallocks,noresvport,noac", port, port)
		if !writable {
			opts += ",rdonly"
		}
	case "linux":
		opts = fmt.Sprintf("port=%d,mountport=%d,vers=3,tcp,local_lock=all,nolock,noac", port, port)
		if !writable {
			opts += ",ro"
		}
	default:
		return "", fmt.Errorf("unsupported OS: %s", goos)
	}
	if extraOpts != "" {
		opts += "," + extraOpts
	}
	return opts, nil
}

// Mount calls the system mount command to mount the NFS server at mountpoint.
// Requires sudo on macOS. The writable flag controls read-only vs read-write.
// extraOpts is appended verbatim to the mount options string (comma-separated).
func Mount(port int, mountpoint string, writable bool, extraOpts string) error {
	opts, err := BuildMountOpts(runtime.GOOS, port, writable, extraOpts)
	if err != nil {
		return err
	}

	cmd := exec.Command("sudo", "mount", "-t", "nfs",
		"-o", opts,
		"localhost:/", mountpoint)
	cmd.Stdin = nil // sudo may need terminal for password
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mount failed: %w\n%s", err, string(output))
	}
	return nil
}

// Unmount calls the system unmount command on the mountpoint.
func Unmount(mountpoint string) error {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "darwin":
		// Try diskutil first (no sudo needed for user NFS mounts)
		cmd = exec.Command("diskutil", "unmount", mountpoint)
		if err := cmd.Run(); err == nil {
			return nil
		}
		// Fallback to sudo umount
		cmd = exec.Command("sudo", "umount", mountpoint)
	default:
		cmd = exec.Command("sudo", "umount", mountpoint)
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("unmount failed: %w\n%s", err, string(output))
	}
	return nil
}
