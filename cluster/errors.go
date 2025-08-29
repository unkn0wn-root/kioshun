package cluster

import "errors"

var (
	ErrNoOwner      = errors.New("no owner for key")
	ErrTimeout      = errors.New("timeout")
	ErrClosed       = errors.New("cluster closed")
	ErrBadPeer      = errors.New("bad peer response")
	ErrNoLoader     = errors.New("no loader configured on primary")
	ErrLeaseTimeout = errors.New("lease timeout")
)
