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

type readTunNode struct {
	fanout        int
	hedgeDelay    time.Duration
	hedgeInterval time.Duration
	perTry        time.Duration
}

func (n *Node[K, V]) rtn() readTunNode {
	t := readTunNode{
		fanout:        n.cfg.ReadMaxFanout,
		hedgeDelay:    n.cfg.ReadHedgeDelay,
		hedgeInterval: n.cfg.ReadHedgeInterval,
		perTry:        n.cfg.ReadPerTryTimeout,
	}
	if t.fanout <= 0 {
		t.fanout = 2
	}
	if t.hedgeDelay <= 0 {
		t.hedgeDelay = 3 * time.Millisecond
	}
	if t.hedgeInterval <= 0 {
		t.hedgeInterval = t.hedgeDelay
	}
	if t.perTry <= 0 {
		t.perTry = 200 * time.Millisecond
	}
	if rt := n.cfg.Sec.ReadTimeout; rt > 0 && t.perTry > rt {
		t.perTry = rt
	}
	return t
}

func (n *Node[K, V]) lwwSetVersion(bk []byte, ver uint64) {
	n.verMu.Lock()
	n.version[string(bk)] = ver
	n.verMu.Unlock()
}

func absExpiry(ttl time.Duration) int64 {
	if ttl <= 0 {
		return 0
	}
	return time.Now().Add(ttl).UnixNano()
}

// Get: read-only, owner-routed read.
// - If present locally, returns immediately.
// - On miss: routes to the primary owner (with optional hedging) and decodes the response.
// - No side effects: does NOT write, replicate, or repair on a miss.
// - Use when you do not want population on miss (you have separate writers or are fine returning not-found).
func (n *Node[K, V]) Get(ctx context.Context, key K) (V, bool, error) {
	var zero V
	owners := n.ownersFor(key)
	bk := n.kc.EncodeKey(key)

	defer n.heat.sample(bk)
	if v, ok := n.local.Get(key); ok {
		return v, true, nil
	}

	if len(owners) == 0 {
		return zero, false, ErrNoOwner
	}

	fast, slow := make([]*nodeMeta, 0, len(owners)), make([]*nodeMeta, 0, len(owners))
	for _, o := range owners {
		if o.Addr == n.cfg.PublicURL {
			continue
		}
		if pc := n.getPeer(o.Addr); pc != nil && !pc.penalized() {
			fast = append(fast, o)
		} else {
			slow = append(slow, o)
		}
	}
	cands := append(fast, slow...)
	if len(cands) == 0 {
		return zero, false, nil
	}

	tu := n.rtn()
	maxFanout := tu.fanout
	if maxFanout > len(cands) {
		maxFanout = len(cands)
	}
	hdl, hival, ptr := tu.hedgeDelay, tu.hedgeInterval, tu.perTry

	type ans struct {
		hit bool
		val []byte
		cp  bool
		err error
	}
	resCh := make(chan ans, len(cands))

	startLeg := func(i int) {
		pc := n.getPeer(cands[i].Addr)
		if pc == nil {
			resCh <- ans{err: ErrBadPeer}
			return
		}

		id := n.nextReqID()
		msg := &MsgGet{Base: Base{T: MTGet, ID: id}, Key: bk}
		raw, err := pc.request(msg, id, ptr)
		if err != nil {
			if errors.Is(err, ErrPeerClosed) {
				n.resetPeer(cands[i].Addr) // next getPeer() will redial
			}
			resCh <- ans{err: err}
			return
		}

		var r MsgGetResp
		if e := cbor.Unmarshal(raw, &r); e != nil {
			resCh <- ans{err: e}
			return
		}

		if r.Err != "" && r.Err != ErrNotFound {
			resCh <- ans{err: errors.New(r.Err)}
			return
		}
		if r.Err == ErrNotFound || !r.Found {
			resCh <- ans{} // miss: no hit, no error
			return
		}

		resCh <- ans{hit: true, val: r.Val, cp: r.Cp}
	}

	// start first leg immediately, then stagger more
	nextIdx := 0
	inflight := 0
	startLeg(nextIdx)
	nextIdx++
	inflight++

	initial := time.NewTimer(hdl)
	defer initial.Stop()
	var ticker *time.Ticker

	for {
		select {
		case <-ctx.Done():
			return zero, false, ctx.Err()

		case r := <-resCh:
			inflight--
			if r.hit {
				vb, err := n.maybeDecompress(r.val, r.cp)
				if err != nil {
					return zero, false, err
				}
				v, err := n.codec.Decode(vb)
				if err != nil {
					return zero, false, err
				}
				return v, true, nil
			}

			// keep pipeline filled up to maxFanout
			if nextIdx < len(cands) && inflight < maxFanout {
				startLeg(nextIdx)
				nextIdx++
				inflight++
			} else if inflight == 0 && nextIdx >= len(cands) {
				return zero, false, nil
			}

		case <-initial.C:
			// after first delay, start more up to maxFanout, spaced by hedge interval
			for inflight < maxFanout && nextIdx < len(cands) {
				startLeg(nextIdx)
				nextIdx++
				inflight++
				if inflight < maxFanout && nextIdx < len(cands) {
					if ticker == nil {
						ticker = time.NewTicker(hival)
						defer ticker.Stop()
					}
					select {
					case <-ticker.C:
					case <-ctx.Done():
						return zero, false, ctx.Err()
					}
				}
			}
		}
	}
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
			n.lwwSetVersion(bk, ver)
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
			n.lwwSetVersion(bk, ver)
		}
	}
	return n.repl.replicateDelete(ctx, bk, owners, ver)
}

// GetOrLoad: read-through with stampede protection and replication.
// - If present locally, returns immediately.
// - Otherwise, coordinates a single load on the primary owner via a lease:
//   - If THIS node is primary, runs the provided loader, Set locally, and replicates to RF owners.
//   - If a REMOTE node is primary, sends LeaseLoad. The remote primary runs its node.Loader
//     (not the loader passed here), Set locally there, and replicates.
//
// - Prevents thundering herd: concurrent callers wait on the lease, so only one load+replicate happens.
// - Use when you want population on miss and cluster-wide propagation with TTL.
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

		if _, acquired := n.leases.acquire(keyStr); acquired {
			var v V
			var ttl time.Duration
			var err error
			defer func() { n.leases.release(keyStr, err) }()

			v, ttl, err = loader(ctx)
			if err == nil {
				_ = n.local.Set(key, v, ttl)
				bv, _ := n.codec.Encode(v)
				ver := n.clock.Next()
				_ = n.repl.replicateSet(ctx, bk, bv, absExpiry(ttl), ver, owners)
				if n.cfg.LWWEnabled {
					n.lwwSetVersion(bk, ver)
				}
			}
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

	ttm := 5 * time.Second
	if rt := n.cfg.Sec.ReadTimeout; rt > 0 && ttm > rt {
		ttm = rt
	}

	id := n.nextReqID()
	msg := &MsgLeaseLoad{Base: Base{T: MTLeaseLoad, ID: id}, Key: bk}
	raw, err := p.request(msg, id, ttm)
	if err != nil {
		if errors.Is(err, ErrPeerClosed) {
			n.resetPeer(primary.Addr)
		}
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
