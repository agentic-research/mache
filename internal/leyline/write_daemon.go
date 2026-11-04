package leyline

import (
	"errors"
	"fmt"
)

// ErrNoWritableDaemon is a write refused because no daemon mache can propose
// to serves the tree being written.
var ErrNoWritableDaemon = errors.New("no leyline daemon serves this tree for writing")

// WritableDaemon returns a client for the daemon a write to root proposes
// through: the one this process spawned with --source root. A propose's paths
// are relative to the daemon's --source, so a daemon serving another tree
// would apply the write somewhere else.
//
// A daemon mache adopted is refused: its --source is not reported by any op
// (ley-line-open-246526 asks for it in status), and a write must not guess.
// Set root with SetDaemonSource before the daemon starts; a daemon already
// running for another root is not restarted, and the write is refused.
func WritableDaemon(root string) (*SocketClient, error) {
	want := CanonicalSourceRoot(root)
	if want == "" {
		return nil, fmt.Errorf("%w: no source root given", ErrNoWritableDaemon)
	}
	sock, err := DiscoverOrStart()
	if err != nil {
		return nil, err
	}
	managed.mu.Lock()
	ownSock, source := managed.sock, managed.source
	managed.mu.Unlock()
	switch {
	case sock != ownSock:
		return nil, fmt.Errorf("%w: the daemon at %s was not started by this mache, and its --source cannot be asked (ley-line-open-246526)",
			ErrNoWritableDaemon, sock)
	case source != want:
		return nil, fmt.Errorf("%w: the daemon serves %q, the write is to %q", ErrNoWritableDaemon, source, want)
	}
	return DialSocket(sock)
}
