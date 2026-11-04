package leyline

import (
	"errors"
	"fmt"
)

// ErrNoWritableDaemon is a write refused because no daemon mache can propose
// to serves the tree being written.
var ErrNoWritableDaemon = errors.New("no leyline daemon serves this tree for writing")

// WritableDaemon returns a client for the daemon a write to root proposes
// through: the pinned daemon, provided it says it serves root.
//
// A propose's paths are relative to the daemon's --source, so a daemon serving
// another tree would apply the write somewhere else. Whether mache spawned the
// daemon or adopted one already running, it is asked the same question: status
// reports its source_root, canonical (ley-line-open-246526), and only an exact
// match with root, resolved the same way, is written through. A daemon with no
// --source has no tree to propose into. A daemon already running for another
// root is not restarted; the write is refused, naming both trees.
func WritableDaemon(root string) (*SocketClient, error) {
	want := CanonicalSourceRoot(root)
	if want == "" {
		return nil, fmt.Errorf("%w: no source root given", ErrNoWritableDaemon)
	}
	sock, err := DiscoverOrStart()
	if err != nil {
		return nil, err
	}
	c, err := DialSocket(sock)
	if err != nil {
		return nil, err
	}
	served, ok, err := c.SourceRoot()
	switch {
	case err != nil:
		_ = c.Close()
		return nil, err
	case !ok:
		_ = c.Close()
		return nil, fmt.Errorf("%w: the daemon at %s runs without --source, the write is to %q", ErrNoWritableDaemon, sock, want)
	case served != want:
		_ = c.Close()
		return nil, fmt.Errorf("%w: the daemon at %s serves %q, the write is to %q", ErrNoWritableDaemon, sock, served, want)
	}
	return c, nil
}
