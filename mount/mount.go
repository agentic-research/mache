// Package mount provides the public NFS mount API for mache.
//
// Types are defined in internal/nfsmount and re-exported here so that
// external consumers (e.g. x-ray) can mount a graph as a real NFS
// filesystem without importing internal packages.
package mount

import (
	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/nfsmount"
)

// Options configures the NFS mount.
type Options struct {
	// ExtraNFSOpts is appended verbatim to the default NFS mount options
	// (comma-separated, e.g. "rsize=32768,wsize=32768").
	ExtraNFSOpts string
}

// Server wraps an NFS server lifecycle.
type Server struct{ inner *nfsmount.Server }

// Port returns the TCP port the NFS server is listening on.
func (s *Server) Port() int { return s.inner.Port() }

// Close stops the NFS server.
func (s *Server) Close() error { return s.inner.Close() }

// NFS starts an NFS server for the given graph and mounts it at mountPoint.
// The caller must Close() the server and Unmount() the mountPoint on cleanup.
// Pass nil for opts to use defaults.
func NFS(g graph.Graph, mountPoint string, opts *Options) (*Server, error) {
	gfs := nfsmount.NewGraphFS(g, &api.Topology{Version: "v1"})
	srv, err := nfsmount.NewServer(gfs)
	if err != nil {
		return nil, err
	}
	var extraOpts string
	if opts != nil {
		extraOpts = opts.ExtraNFSOpts
	}
	if err := nfsmount.Mount(srv.Port(), mountPoint, false, extraOpts); err != nil {
		_ = srv.Close()
		return nil, err
	}
	return &Server{inner: srv}, nil
}

// Unmount unmounts the given mount point.
func Unmount(mountPoint string) error {
	return nfsmount.Unmount(mountPoint)
}
