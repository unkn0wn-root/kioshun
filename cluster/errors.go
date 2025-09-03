package cluster

import (
	"errors"
	"io"
	"net"
	"syscall"
)

var (
	ErrNoOwner      = errors.New("no owner for key")
	ErrTimeout      = errors.New("timeout")
	ErrClosed       = errors.New("cluster closed")
	ErrBadPeer      = errors.New("bad peer response")
	ErrNoLoader     = errors.New("no loader configured on primary")
	ErrLeaseTimeout = errors.New("lease timeout")
	ErrPeerClosed   = errors.New("peer closed")
)

// isFatalTransport reports whether an error indicates a broken or unusable
// transport that should trigger a peer reset/redial.
// Timeouts and application errors are considered non-fatal.
func isFatalTransport(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, ErrTimeout) {
		return false
	}

	if errors.Is(err, ErrPeerClosed) || errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) {
		return true
	}

	var nerr net.Error
	if errors.As(err, &nerr) {
		return !nerr.Timeout()
	}

	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNABORTED) {
		return true
	}
	return false
}
