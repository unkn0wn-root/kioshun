package cluster

import (
	"context"
	"errors"
	"time"

	cbor "github.com/fxamacker/cbor/v2"
)

type DistCache[K comparable, V any] interface {
	Get(ctx context.Context, key K) (V, bool, error)
	Set(ctx context.Context, key K, val V, ttl time.Duration) error
	Delete(ctx context.Context, key K) error
	GetOrLoad(ctx context.Context, key K, loader func(context.Context) (V, time.Duration, error)) (V, error)
}

// Get performs an owner-routed read. It prefers the local shard when this
// node is primary, otherwise it contacts the primary owner over the wire and
// decodes the response (with optional compression).
func (n *Node[K, V]) Get(ctx context.Context, key K) (V, bool, error) {
	var zero V
	owners := n.ownersFor(key)
	bk := n.kc.EncodeKey(key)
	for _, own := range owners {
		if own.Addr == n.cfg.PublicURL {
			if v, ok := n.local.Get(key); ok {
				n.heat.sample(bk)
				return v, true, nil
			}
			n.heat.sample(bk)
			break
		}
	}

	if len(owners) == 0 {
		return zero, false, ErrNoOwner
	}

	// Primary is first owner. We try it and rely on transport backpressure.
	p := n.getPeer(owners[0].Addr)
	if p == nil {
		return zero, false, ErrBadPeer
	}

	id := n.nextReqID()
	msg := &MsgGet{Base: Base{T: MTGet, ID: id}, Key: bk}
	raw, err := p.request(msg, id, n.cfg.Sec.ReadTimeout)
	if err != nil {
		return zero, false, err
	}

	var resp MsgGetResp
	if err := cbor.Unmarshal(raw, &resp); err != nil {
		return zero, false, err
	}
	if resp.Err != "" {
		return zero, false, errors.New(resp.Err)
	}
	if !resp.Found {
		return zero, false, nil
	}

	valBytes, err := n.maybeDecompress(resp.Val, resp.Cp)
	if err != nil {
		return zero, false, err
	}

	v, err := n.codec.Decode(valBytes)
	if err != nil {
		return zero, false, err
	}
	return v, true, nil
}

func (n *Node[K, V]) Set(ctx context.Context, key K, val V, ttl time.Duration) error {
	bk := n.kc.EncodeKey(key)
	if n.cfg.Sec.MaxKeySize > 0 && len(bk) > n.cfg.Sec.MaxKeySize {
		return errors.New("key too large")
	}

	owners := n.ownersFor(key)
	if len(owners) == 0 {
		return ErrNoOwner
	}

	bv, err := n.codec.Encode(val)
	if err != nil {
		return err
	}

	if n.cfg.Sec.MaxValueSize > 0 && len(bv) > n.cfg.Sec.MaxValueSize {
		return errors.New("value too large")
	}

	exp := int64(0)
	if ttl > 0 {
		exp = time.Now().Add(ttl).UnixNano()
	}

	ver := n.clock.Next()
	if owners[0].Addr == n.cfg.PublicURL {
		_ = n.local.Set(key, val, ttl)
		if n.cfg.LWWEnabled {
			n.verMu.Lock()
			n.version[string(bk)] = ver
			n.verMu.Unlock()
		}
	}
	return n.repl.replicateSet(ctx, bk, bv, exp, ver, owners)
}

func (n *Node[K, V]) Delete(ctx context.Context, key K) error {
	bk := n.kc.EncodeKey(key)
	if n.cfg.Sec.MaxKeySize > 0 && len(bk) > n.cfg.Sec.MaxKeySize {
		return errors.New("key too large")
	}

	owners := n.ownersFor(key)
	if len(owners) == 0 {
		return ErrNoOwner
	}

	ver := n.clock.Next()
	if owners[0].Addr == n.cfg.PublicURL {
		n.local.Delete(key)
		if n.cfg.LWWEnabled {
			n.verMu.Lock()
			n.version[string(bk)] = ver
			n.verMu.Unlock()
		}
	}
	return n.repl.replicateDelete(ctx, bk, owners, ver)
}

func (n *Node[K, V]) GetOrLoad(ctx context.Context, key K, loader func(context.Context) (V, time.Duration, error)) (V, error) {
	var zero V
	if v, ok := n.local.Get(key); ok {
		return v, nil
	}

	owners := n.ownersFor(key)
	if len(owners) == 0 {
		return zero, ErrNoOwner
	}

	bk := n.kc.EncodeKey(key)
	keyStr := string(bk)
	primary := owners[0]
	if primary.Addr == n.cfg.PublicURL {
		_, acquired := n.leases.acquire(keyStr)
		if acquired {
			v, ttl, err := loader(ctx)
			if err == nil {
				_ = n.local.Set(key, v, ttl)
				bv, _ := n.codec.Encode(v)
				exp := int64(0)
				if ttl > 0 {
					exp = time.Now().Add(ttl).UnixNano()
				}

				ver := n.clock.Next()
				_ = n.repl.replicateSet(ctx, bk, bv, exp, ver, owners)
				if n.cfg.LWWEnabled {
					n.verMu.Lock()
					n.version[keyStr] = ver
					n.verMu.Unlock()
				}
			}
			n.leases.release(keyStr, err)
			return v, err
		}

		if err := n.leases.wait(ctx, keyStr); err != nil {
			return zero, err
		}

		if v, ok := n.local.Get(key); ok {
			return v, nil
		}
		return zero, errors.New("lease resolved but key absent")
	}

	// delegate to primary to enforce single-flight via lease table there.
	p := n.getPeer(primary.Addr)
	if p == nil {
		return zero, ErrBadPeer
	}

	id := n.nextReqID()
	msg := &MsgLeaseLoad{Base: Base{T: MTLeaseLoad, ID: id}, Key: bk}
	raw, err := p.request(msg, id, 5*time.Second)
	if err != nil {
		return zero, err
	}

	var resp MsgLeaseLoadResp
	if err := cbor.Unmarshal(raw, &resp); err != nil {
		return zero, err
	}
	if resp.Err != "" {
		return zero, errors.New(resp.Err)
	}
	if !resp.Found {
		return zero, errors.New("loader returned no value")
	}

	valBytes, err := n.maybeDecompress(resp.Val, resp.Cp)
	if err != nil {
		return zero, err
	}

	v, err := n.codec.Decode(valBytes)
	if err != nil {
		return zero, err
	}
	return v, nil
}
