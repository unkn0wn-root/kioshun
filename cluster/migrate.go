package cluster

import (
	"time"

	cbor "github.com/fxamacker/cbor/v2"
)

// rebalancerLoop periodically runs a bounded rebalance pass that migrates
// locally owned-but-not-primary keys to their current primary owner.
func (n *Node[K, V]) rebalancerLoop() {
	iv := n.cfg.RebalanceInterval
	if iv <= 0 {
		return
	}

	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			n.rebalanceOnce()
		case <-n.stop:
			return
		}
	}
}

// rebalanceOnce scans up to RebalanceLimit local keys and, for keys whose
// primary owner moved away, pushes their latest value to the new primary and
// deletes the local copy on success.
func (n *Node[K, V]) rebalanceOnce() {
	keys := n.local.Keys()
	if len(keys) == 0 {
		return
	}

	limit := n.cfg.RebalanceLimit
	if limit <= 0 || limit > len(keys) {
		limit = len(keys)
	}

	for i := 0; i < limit; i++ {
		k := keys[i]
		owners := n.ownersFor(k)
		if len(owners) == 0 {
			continue
		}

		primary := owners[0]
		if primary.ID == n.cfg.ID {
			continue
		}

		v, ttl, ok := n.local.GetWithTTL(k)
		if !ok {
			continue
		}

		// Encode + (maybe) compress once.
		vb, err := n.codec.Encode(v)
		if err != nil {
			continue
		}
		vb, cp := n.maybeCompress(vb)

		bk := n.kc.EncodeKey(k)
		exp := absExpiry(ttl)
		pc := n.getPeer(primary.ID)
		if pc == nil || pc.penalized() {
			// Let next pass try again; we keep local until success.
			continue
		}

		id := n.nextReqID()
		ver := n.clock.Next()
		msg := &MsgSet{
			Base: Base{
				T:  MTSet,
				ID: id,
			},
			Key: bk,
			Val: vb,
			Exp: exp,
			Ver: ver,
			Cp:  cp,
		}

		raw, err := pc.request(msg, id, n.cfg.Sec.WriteTimeout)
		if err != nil {
			if isFatalTransport(err) {
				n.resetPeer(primary.ID)
			}
			continue
		}

		var resp MsgSetResp
		if e := cbor.Unmarshal(raw, &resp); e != nil {
			n.resetPeer(primary.ID)
			continue
		}
		if !resp.OK {
			continue
		}
		n.local.Delete(k)
	}
}
