package cluster

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	cbor "github.com/fxamacker/cbor/v2"
)

type replicator[K comparable, V any] struct {
	node *Node[K, V]
}

// replicateSet sends a write to all owners and waits for WC acknowledgements.
//   - Pre-compresses the value once per request to avoid per-peer work.
//   - Enqueues hinted-handoff on any peer failure so a recovering node can catch up.
//   - Fast path: if local commit already satisfies WC, fire-and-forget to peers and
//     rely on hinted handoff to close any gaps, the caller is unblocked.
func (r *replicator[K, V]) replicateSet(ctx context.Context, key []byte, val []byte, exp int64, ver uint64, owners []*nodeMeta) error {
	required := r.node.cfg.WriteConcern
	if required < 1 {
		required = 1
	}

	acks := 0
	if len(owners) > 0 && owners[0].ID == r.node.cfg.ID {
		acks++
	}
	want := required - acks

	// pre-compress once for all peers.
	b2, cp := r.node.maybeCompress(val)

	// helper to send and enqueue hint on failure
	sendOne := func(pid NodeID, pc *peerConn) {
		if pc == nil {
			if r.node.hh != nil {
				r.node.hh.enqueueSet(pid, key, b2, exp, ver, cp)
			}
			return
		}

		reqID := r.node.nextReqID()
		msg := &MsgSet{Base: Base{T: MTSet, ID: reqID}, Key: key, Val: b2, Exp: exp, Ver: ver, Cp: cp}
		raw, err := pc.request(msg, reqID, r.node.cfg.Sec.WriteTimeout)
		if err != nil {
			if r.node.hh != nil {
				r.node.hh.enqueueSet(pid, key, b2, exp, ver, cp)
			}
			return
		}

		var resp MsgSetResp
		if e := cbor.Unmarshal(raw, &resp); e != nil || !resp.OK {
			if r.node.hh != nil {
				r.node.hh.enqueueSet(pid, key, b2, exp, ver, cp)
			}
			return
		}
	}

	// Fast path: local already satisfies WC. Fire and forget to remaining owners,
	// still capturing failures into hinted handoff without blocking the caller.
	if want <= 0 {
		for _, own := range owners {
			if own.ID == r.node.cfg.ID {
				continue
			}
			pc := r.node.getPeer(own.ID)
			go sendOne(own.ID, pc)
		}
		return nil
	}

	// slow path: need acknowledgements from peers to meet write concern.
	remaining := int32(want)
	timer := time.NewTimer(r.node.cfg.Sec.WriteTimeout + time.Second)
	defer timer.Stop()

	errCh := make(chan error, len(owners))
	for _, own := range owners {
		if own.ID == r.node.cfg.ID {
			continue
		}
		pc := r.node.getPeer(own.ID)
		if pc == nil {
			// we know this one will miss; enqueue and continue
			if r.node.hh != nil {
				r.node.hh.enqueueSet(own.ID, key, b2, exp, ver, cp)
			}
			continue
		}

		go func(pid NodeID, p *peerConn) {
			reqID := r.node.nextReqID()
			msg := &MsgSet{Base: Base{T: MTSet, ID: reqID}, Key: key, Val: b2, Exp: exp, Ver: ver, Cp: cp}
			raw, err := p.request(msg, reqID, r.node.cfg.Sec.WriteTimeout)
			if err != nil {
				// enqueue and return an error
				if r.node.hh != nil {
					r.node.hh.enqueueSet(pid, key, b2, exp, ver, cp)
				}
				errCh <- err
				return
			}

			var resp MsgSetResp
			if e := cbor.Unmarshal(raw, &resp); e != nil || !resp.OK {
				if r.node.hh != nil {
					r.node.hh.enqueueSet(pid, key, b2, exp, ver, cp)
				}
				if e == nil && resp.Err != "" {
					errCh <- errors.New(resp.Err)
				} else {
					errCh <- errors.New("set not ok")
				}
				return
			}
			// success
			if atomic.AddInt32(&remaining, -1) <= 0 {
				errCh <- nil
			}
		}(own.ID, pc)
	}

	for {
		select {
		case err := <-errCh:
			if err == nil {
				return nil // met write concern
			}
			// keep waiting for success until timeout; errors alone don't abort early
		case <-timer.C:
			return ErrTimeout
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (r *replicator[K, V]) replicateDelete(ctx context.Context, key []byte, owners []*nodeMeta, ver uint64) error {
	required := r.node.cfg.WriteConcern
	if required < 1 {
		required = 1
	}
	acks := 0
	if len(owners) > 0 && owners[0].ID == r.node.cfg.ID {
		acks++
	}
	want := required - acks

	sendOne := func(pid NodeID, pc *peerConn) {
		if pc == nil {
			if r.node.hh != nil {
				r.node.hh.enqueueDel(pid, key, ver)
			}
			return
		}

		reqID := r.node.nextReqID()
		msg := &MsgDel{Base: Base{T: MTDelete, ID: reqID}, Key: key, Ver: ver}
		raw, err := pc.request(msg, reqID, r.node.cfg.Sec.WriteTimeout)
		if err != nil {
			if r.node.hh != nil {
				r.node.hh.enqueueDel(pid, key, ver)
			}
			return
		}

		var resp MsgDelResp
		if e := cbor.Unmarshal(raw, &resp); e != nil || !resp.OK {
			if r.node.hh != nil {
				r.node.hh.enqueueDel(pid, key, ver)
			}
			return
		}
	}

	if want <= 0 {
		for _, own := range owners {
			if own.ID == r.node.cfg.ID {
				continue
			}
			pc := r.node.getPeer(own.ID)
			go sendOne(own.ID, pc)
		}
		return nil
	}

	remaining := int32(want)
	timer := time.NewTimer(r.node.cfg.Sec.WriteTimeout + time.Second)
	defer timer.Stop()
	errCh := make(chan error, len(owners))

	for _, own := range owners {
		if own.ID == r.node.cfg.ID {
			continue
		}
		pc := r.node.getPeer(own.ID)
		if pc == nil {
			if r.node.hh != nil {
				r.node.hh.enqueueDel(own.ID, key, ver)
			}
			continue
		}

		go func(pid NodeID, p *peerConn) {
			reqID := r.node.nextReqID()
			msg := &MsgDel{Base: Base{T: MTDelete, ID: reqID}, Key: key, Ver: ver}
			raw, err := p.request(msg, reqID, r.node.cfg.Sec.WriteTimeout)
			if err != nil {
				if r.node.hh != nil {
					r.node.hh.enqueueDel(pid, key, ver)
				}
				errCh <- err
				return
			}

			var resp MsgDelResp
			if e := cbor.Unmarshal(raw, &resp); e != nil || !resp.OK {
				if r.node.hh != nil {
					r.node.hh.enqueueDel(pid, key, ver)
				}
				if e == nil && resp.Err != "" {
					errCh <- errors.New(resp.Err)
				} else {
					errCh <- errors.New("delete not ok")
				}
				return
			}
			if atomic.AddInt32(&remaining, -1) <= 0 {
				errCh <- nil
			}
		}(own.ID, pc)
	}

	for {
		select {
		case err := <-errCh:
			if err == nil {
				return nil
			}
		case <-timer.C:
			return ErrTimeout
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
