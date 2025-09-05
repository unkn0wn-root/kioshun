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

// rtn resolves read-tuning parameters (fanout, hedging cadence, and per-try timeout)
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

// lwwSetVersion records the latest observed version for a key under LWW.
func (n *Node[K, V]) lwwSetVersion(bk []byte, ver uint64) {
	n.verMu.Lock()
	n.version[string(bk)] = ver
	n.verMu.Unlock()
}

// absExpiry converts a TTL to an absolute expiration in UnixNano; 0 if none.
func absExpiry(ttl time.Duration) int64 {
	if ttl <= 0 {
		return 0
	}
	return time.Now().Add(ttl).UnixNano()
}

// Get is an owner-routed read with optional hedging.
func (n *Node[K, V]) Get(ctx context.Context, key K) (V, bool, error) {
	var zero V

	bk := n.kc.EncodeKey(key)
	defer n.heat.sample(bk)

	// route by exact RF owners from the ring
	h64 := n.hash64Of(key)
	r := n.ring.Load().(*ring)
	owners := r.ownersFromKeyHash(h64)
	if len(owners) == 0 {
		return zero, false, ErrNoOwner
	}
	if owners[0].Addr == n.cfg.PublicURL {
		if v, ok := n.local.Get(key); ok {
			return v, true, nil
		}
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
		// No remote legs to try (we're the only owner and local miss) -> miss.
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

	// track whether at least one leg completed cleanly (hit or miss)
	var lastErr error
	clean := false
	hadErr := false

	startLeg := func(i int) {
		addr := cands[i].Addr
		pc := n.getPeer(addr)
		if pc == nil {
			resCh <- ans{err: ErrBadPeer}
			return
		}

		id := n.nextReqID()
		msg := &MsgGet{Base: Base{T: MTGet, ID: id}, Key: bk}
		raw, err := pc.request(msg, id, ptr)
		if err != nil {
			if isFatalTransport(err) {
				n.resetPeer(addr) // next getPeer() will redial
			}
			resCh <- ans{err: err}
			return
		}

		var rmsg MsgGetResp
		if e := cbor.Unmarshal(raw, &rmsg); e != nil {
			// Broken stream -> reset peer
			n.resetPeer(addr)
			resCh <- ans{err: e}
			return
		}

		// Be tolerant and treat "ErrNotFound" as a clean miss.
		if rmsg.Err != "" {
			if rmsg.Err == ErrNotFound {
				resCh <- ans{}
				return
			}
			resCh <- ans{err: errors.New(rmsg.Err)}
			return
		}
		if !rmsg.Found {
			resCh <- ans{}
			return
		}
		resCh <- ans{hit: true, val: rmsg.Val, cp: rmsg.Cp}
	}

	// Start first leg, then hedge
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

		case rans := <-resCh:
			inflight--
			if rans.err != nil {
				lastErr = rans.err
				hadErr = true
			} else {
				clean = true // either a hit or a clean miss
			}

			if rans.hit {
				vb, err := n.maybeDecompress(rans.val, rans.cp)
				if err != nil {
					return zero, false, err
				}
				v, err := n.codec.Decode(vb)
				if err != nil {
					return zero, false, err
				}
				return v, true, nil
			}

			// Keep pipeline filled up to maxFanout
			if nextIdx < len(cands) && inflight < maxFanout {
				startLeg(nextIdx)
				nextIdx++
				inflight++
			} else if inflight == 0 && nextIdx >= len(cands) {
				// All legs finished
				if !clean && hadErr {
					return zero, false, lastErr // every leg errored
				}
				return zero, false, nil // miss
			}

		case <-initial.C:
			// After first delay, start more up to maxFanout, spaced by hedge interval
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

// Set writes the value with TTL, assigning a new HLC version and replicating.
func (n *Node[K, V]) Set(ctx context.Context, key K, val V, ttl time.Duration) error {
	bk := n.kc.EncodeKey(key)
	if n.cfg.Sec.MaxKeySize > 0 && len(bk) > n.cfg.Sec.MaxKeySize {
		return errors.New("key too large")
	}

	// Route by exact RF owners
	h64 := n.hash64Of(key)
	r := n.ring.Load().(*ring)
	owners := r.ownersFromKeyHash(h64)
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

	exp := absExpiry(ttl)
	ver := n.clock.Next()

	if owners[0].Addr == n.cfg.PublicURL {
		_ = n.local.Set(key, val, ttl)
		if n.cfg.LWWEnabled {
			n.lwwSetVersion(bk, ver)
		}
	}
	return n.repl.replicateSet(ctx, bk, bv, exp, ver, owners)
}

// Delete removes key cluster-wide by assigning a tombstone version.
func (n *Node[K, V]) Delete(ctx context.Context, key K) error {
	bk := n.kc.EncodeKey(key)
	if n.cfg.Sec.MaxKeySize > 0 && len(bk) > n.cfg.Sec.MaxKeySize {
		return errors.New("key too large")
	}

	// Route by exact RF owners
	h64 := n.hash64Of(key)
	r := n.ring.Load().(*ring)
	owners := r.ownersFromKeyHash(h64)
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

// GetOrLoad is read-through with stampede protection and replication.
func (n *Node[K, V]) GetOrLoad(ctx context.Context, key K, loader func(context.Context) (V, time.Duration, error)) (V, error) {
	var zero V
	if v, ok := n.local.Get(key); ok {
		return v, nil
	}

	// Route by exact RF owners
	h64 := n.hash64Of(key)
	r := n.ring.Load().(*ring)
	owners := r.ownersFromKeyHash(h64)
	if len(owners) == 0 {
		return zero, ErrNoOwner
	}

	bk := n.kc.EncodeKey(key)
	keyStr := string(bk)
	primary := owners[0]

	if primary.Addr == n.cfg.PublicURL {
		// We are primary: single-flight locally.
		if _, acquired := n.leases.acquire(keyStr); acquired {
			var v V
			var ttl time.Duration
			var err error
			defer func() { n.leases.release(keyStr, err) }()

			v, ttl, err = loader(ctx)
			if err == nil {
				_ = n.local.Set(key, v, ttl)

				bv, encErr := n.codec.Encode(v)
				if encErr != nil {
					err = encErr
					return zero, err
				}
				if n.cfg.Sec.MaxValueSize > 0 && len(bv) > n.cfg.Sec.MaxValueSize {
					err = errors.New("value too large")
					return zero, err
				}

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

	// Delegate to primary (its lease table coordinates the load).
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
		if isFatalTransport(err) {
			n.resetPeer(primary.Addr)
		}
		return zero, err
	}

	var resp MsgLeaseLoadResp
	if err := cbor.Unmarshal(raw, &resp); err != nil {
		n.resetPeer(primary.Addr)
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
