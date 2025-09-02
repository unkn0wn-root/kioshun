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
	if len(owners) > 0 && owners[0].Addr == r.node.cfg.PublicURL {
		acks++
	}
	want := required - acks

	// pre-compress once for all peers.
	b2, cp := r.node.maybeCompress(val)

	// helper to send and enqueue hint on failure
	sendOne := func(addr string, pc *peerConn) {
		if pc == nil {
			if r.node.hh != nil {
				r.node.hh.enqueueSet(addr, key, b2, exp, ver, cp)
			}
			return
		}

		id := r.node.nextReqID()
		msg := &MsgSet{Base: Base{T: MTSet, ID: id}, Key: key, Val: b2, Exp: exp, Ver: ver, Cp: cp}
		raw, err := pc.request(msg, id, r.node.cfg.Sec.WriteTimeout)
		if err != nil {
			if r.node.hh != nil {
				r.node.hh.enqueueSet(addr, key, b2, exp, ver, cp)
			}
			return
		}

		var resp MsgSetResp
		if e := cbor.Unmarshal(raw, &resp); e != nil || !resp.OK {
			if r.node.hh != nil {
				if e == nil && resp.Err != "" {
					// still enqueue; resp.Err examined for logging if needed
				}
				r.node.hh.enqueueSet(addr, key, b2, exp, ver, cp)
			}
			return
		}
	}

	// Fast path: local already satisfies WC. Fire and forget to remaining owners,
	// still capturing failures into hinted handoff without blocking the caller.
	if want <= 0 {
		for _, own := range owners {
			if own.Addr == r.node.cfg.PublicURL {
				continue
			}

			if pc := r.node.getPeer(own.Addr); pc != nil {
				go sendOne(own.Addr, pc)
			} else {
				// enqueue immediately if we don't even have a connection
				if r.node.hh != nil {
					r.node.hh.enqueueSet(own.Addr, key, b2, exp, ver, cp)
				}
			}
		}
		return nil
	}

	// slow path: need acknowledgements from peers to meet write concern.
	remaining := int32(want)
	timer := time.NewTimer(r.node.cfg.Sec.WriteTimeout + time.Second)
	defer timer.Stop()

	errCh := make(chan error, len(owners))
	for _, own := range owners {
		if own.Addr == r.node.cfg.PublicURL {
			continue
		}
		pc := r.node.getPeer(own.Addr)
		if pc == nil {
			// we know this one will miss; enqueue and continue
			if r.node.hh != nil {
				r.node.hh.enqueueSet(own.Addr, key, b2, exp, ver, cp)
			}
			continue
		}

		go func(addr string, p *peerConn) {
			id := r.node.nextReqID()
			msg := &MsgSet{Base: Base{T: MTSet, ID: id}, Key: key, Val: b2, Exp: exp, Ver: ver, Cp: cp}
			raw, err := p.request(msg, id, r.node.cfg.Sec.WriteTimeout)
			if err != nil {
				// enqueue and return an error
				if r.node.hh != nil {
					r.node.hh.enqueueSet(addr, key, b2, exp, ver, cp)
				}
				errCh <- err
				return
			}

			var resp MsgSetResp
			if e := cbor.Unmarshal(raw, &resp); e != nil || !resp.OK {
				if r.node.hh != nil {
					r.node.hh.enqueueSet(addr, key, b2, exp, ver, cp)
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
		}(own.Addr, pc)
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
	if len(owners) > 0 && owners[0].Addr == r.node.cfg.PublicURL {
		acks++
	}
	want := required - acks

	sendOne := func(addr string, pc *peerConn) {
		if pc == nil {
			if r.node.hh != nil {
				r.node.hh.enqueueDel(addr, key, ver)
			}
			return
		}

		id := r.node.nextReqID()
		msg := &MsgDel{Base: Base{T: MTDelete, ID: id}, Key: key, Ver: ver}
		raw, err := pc.request(msg, id, r.node.cfg.Sec.WriteTimeout)
		if err != nil {
			if r.node.hh != nil {
				r.node.hh.enqueueDel(addr, key, ver)
			}
			return
		}

		var resp MsgDelResp
		if e := cbor.Unmarshal(raw, &resp); e != nil || !resp.OK {
			if r.node.hh != nil {
				r.node.hh.enqueueDel(addr, key, ver)
			}
			return
		}
	}

	if want <= 0 {
		for _, own := range owners {
			if own.Addr == r.node.cfg.PublicURL {
				continue
			}
			if pc := r.node.getPeer(own.Addr); pc != nil {
				go sendOne(own.Addr, pc)
			} else {
				if r.node.hh != nil {
					r.node.hh.enqueueDel(own.Addr, key, ver)
				}
			}
		}
		return nil
	}

	remaining := int32(want)
	timer := time.NewTimer(r.node.cfg.Sec.WriteTimeout + time.Second)
	defer timer.Stop()
	errCh := make(chan error, len(owners))

	for _, own := range owners {
		if own.Addr == r.node.cfg.PublicURL {
			continue
		}
		pc := r.node.getPeer(own.Addr)
		if pc == nil {
			if r.node.hh != nil {
				r.node.hh.enqueueDel(own.Addr, key, ver)
			}
			continue
		}

		go func(addr string, p *peerConn) {
			id := r.node.nextReqID()
			msg := &MsgDel{Base: Base{T: MTDelete, ID: id}, Key: key, Ver: ver}
			raw, err := p.request(msg, id, r.node.cfg.Sec.WriteTimeout)
			if err != nil {
				if r.node.hh != nil {
					r.node.hh.enqueueDel(addr, key, ver)
				}
				errCh <- err
				return
			}

			var resp MsgDelResp
			if e := cbor.Unmarshal(raw, &resp); e != nil || !resp.OK {
				if r.node.hh != nil {
					r.node.hh.enqueueDel(addr, key, ver)
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
		}(own.Addr, pc)
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
