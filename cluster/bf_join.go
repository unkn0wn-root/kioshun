package cluster

import (
	"encoding/binary"
	"sort"
	"time"

	"github.com/cespare/xxhash/v2"
	cbor "github.com/fxamacker/cbor/v2"
	cache "github.com/unkn0wn-root/kioshun"
)

type bucketSig struct {
	count uint32
	hash  uint64
}

const defaultBackfillDepth = 2 // 65,536 buckets

// readyPollInterval picks a small poll period relative to configured cadences,
// clamped into a sane range to avoid busy spinning or sluggish startup.
func readyPollInterval(cfg Config) time.Duration {
	p := 150 * time.Millisecond
	if cfg.GossipInterval > 0 && cfg.GossipInterval/4 < p {
		p = cfg.GossipInterval / 4
	}
	if cfg.WeightUpdate > 0 && cfg.WeightUpdate/4 < p {
		p = cfg.WeightUpdate / 4
	}
	if p < 100*time.Millisecond {
		p = 100 * time.Millisecond
	}
	if p > 500*time.Millisecond {
		p = 500 * time.Millisecond
	}
	return p
}

func (n *Node[K, V]) backfillLoop() {
	// Wait for initial membership/ring readiness with a bounded timeout
	// to avoid running a no-op backfill before peers and ring are populated.
	// Conditions to proceed: at least one peer connected OR ring has >1 node.
	timeout := 3 * time.Second
	if n.cfg.GossipInterval > 0 {
		if d := 3 * n.cfg.GossipInterval; d > timeout {
			timeout = d
		}
	}

	if n.cfg.WeightUpdate > 0 {
		if d := 3 * n.cfg.WeightUpdate; d > timeout {
			timeout = d
		}
	}

	deadline := time.Now().Add(timeout)
	poll := readyPollInterval(n.cfg) // typically ~150ms
	tk := time.NewTicker(poll)
	defer tk.Stop()
	for {
		r := n.ring.Load().(*ring)
		if len(n.peerAddrs()) > 0 || len(r.nodes) > 1 {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-tk.C:
		case <-n.stop:
			return
		}
	}

	n.backfillOnce(defaultBackfillDepth, 1024)

	t := time.NewTicker(n.cfg.RebalanceInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			n.backfillOnce(defaultBackfillDepth, 512) // light repair
		case <-n.stop:
			return
		}
	}
}

func (n *Node[K, V]) backfillOnce(depth int, page int) {
	donors := n.peerAddrs()
	self := n.cfg.PublicURL
	tmp := donors[:0]
	for _, d := range donors {
		if d != self && n.getPeer(d) != nil {
			tmp = append(tmp, d)
		}
	}

	donors = tmp
	if len(donors) == 0 {
		return
	}

	// Compute a local view of bucket digests we own to detect divergence.
	local := n.computeLocalDigests(depth)
	sort.Strings(donors)

	for _, d := range donors {
		pc := n.getPeer(d)
		if pc == nil {
			continue
		}

		req := &MsgBackfillDigestReq{Base: Base{T: MTBackfillDigestReq, ID: n.nextReqID()}, Target: self, Depth: uint8(depth)}
		raw, err := pc.request(req, req.ID, n.cfg.Sec.ReadTimeout)
		if err != nil {
			continue
		}

		var dr MsgBackfillDigestResp
		if e := cbor.Unmarshal(raw, &dr); e != nil || len(dr.Buckets) == 0 {
			continue
		}

		for _, b := range dr.Buckets {
			lp := local[string(b.Prefix)]
			if lp.count == b.Count && lp.hash == b.Hash64 {
				continue // already in sync for this bucket
			}

			// Page through differing buckets using last key-hash cursor.
			var cursor []byte
			for {
				kReq := &MsgBackfillKeysReq{
					Base:   Base{T: MTBackfillKeysReq, ID: n.nextReqID()},
					Target: self,
					Prefix: append([]byte(nil), b.Prefix...),
					Limit:  page,
					Cursor: cursor,
				}

				raw2, err := pc.request(kReq, kReq.ID, n.cfg.Sec.ReadTimeout)
				if err != nil {
					break
				}

				var kr MsgBackfillKeysResp
				if e := cbor.Unmarshal(raw2, &kr); e != nil || len(kr.Items) == 0 {
					break
				}

				// decode + import only keys that successfully decoded and passed limits.
				toImport := make([]cache.Item[K, V], 0, len(kr.Items))
				for _, kv := range kr.Items {
					k, err := n.kc.DecodeKey(kv.K)
					if err != nil {
						continue
					}

					vb, err := n.maybeDecompress(kv.V, kv.Cp)
					if err != nil {
						continue
					}

					v, err := n.codec.Decode(vb)
					if err != nil {
						continue
					}

					toImport = append(toImport, cache.Item[K, V]{
						Key:       k,
						Val:       v,
						ExpireAbs: kv.E,
						Version:   kv.Ver,
					})
				}

				if len(toImport) > 0 {
					n.local.Import(toImport)
					if n.cfg.LWWEnabled {
						n.verMu.Lock()
						for _, it := range toImport {
							n.version[string(n.kc.EncodeKey(it.Key))] = it.Version
						}
						n.verMu.Unlock()

						last := toImport[len(toImport)-1].Version
						if last > 0 {
							n.clock.Observe(last)
						}
					}
					local = n.updateLocalDigestWithBatch(local, depth, toImport)
				}

				if len(kr.NextCursor) == 8 {
					cursor = append([]byte(nil), kr.NextCursor...)
				} else {
					break
				}
			}
		}
	}
}

func (n *Node[K, V]) computeLocalDigests(depth int) map[string]bucketSig {
	if depth <= 0 || depth > 8 {
		depth = 2
	}
	m := make(map[string]bucketSig, 1<<12)
	keys := n.local.Keys()
	self := n.cfg.PublicURL

	for _, k := range keys {
		owners := n.ownersFor(k)
		owned := false
		for _, o := range owners {
			if o.Addr == self {
				owned = true
				break
			}
		}
		if !owned {
			continue
		}

		kb := n.kc.EncodeKey(k)
		h64 := xxhash.Sum64(kb)
		var hb [8]byte
		binary.BigEndian.PutUint64(hb[:], h64)
		prefix := string(hb[:depth])

		// include version in the digest to detect divergence even when
		// counts match (XORing hash with version is inexpensive and orderless).
		var ver uint64
		if n.cfg.LWWEnabled {
			n.verMu.RLock()
			ver = n.version[string(kb)]
			n.verMu.RUnlock()
		}
		s := m[prefix]
		s.count++
		s.hash ^= (h64 ^ ver)
		m[prefix] = s
	}
	return m
}

func (n *Node[K, V]) updateLocalDigestWithBatch(m map[string]bucketSig, depth int, batch []cache.Item[K, V]) map[string]bucketSig {
	for _, it := range batch {
		kb := n.kc.EncodeKey(it.Key)
		h64 := xxhash.Sum64(kb)
		var hb [8]byte
		binary.BigEndian.PutUint64(hb[:], h64)
		prefix := string(hb[:depth])
		s := m[prefix]
		s.count++
		s.hash ^= (h64 ^ it.Version)
		m[prefix] = s
	}
	return m
}
