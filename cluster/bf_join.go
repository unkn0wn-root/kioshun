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

// readyPollInterval picks a small poll period relative to configured cadences.
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

// backfillLoop waits until the node has a minimally ready view of the
// cluster (some peers connected or a ring with >1 node), then performs an
// initial state backfill from peers. After startup it periodically runs a
// light repair pass to reconcile keys that may have diverged due to
// membership changes or temporary failures.
// Wait for initial membership/ring readiness with a bounded timeout
// to avoid running a no-op backfill before peers and ring are populated.
// Conditions to proceed: at least one peer connected OR ring has >1 node.
func (n *Node[K, V]) backfillLoop() {
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
		if len(n.peerIDs()) > 0 || len(r.nodes) > 1 {
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

	iv := n.cfg.BackfillInterval
	if iv <= 0 {
		iv = n.cfg.RebalanceInterval
		if iv <= 0 {
			iv = 30 * time.Second
		}
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			n.backfillOnce(defaultBackfillDepth, 512) // light repair
		case <-n.backfillKick:
			// triggered by membership epoch increase -> run a light repair promptly.
			n.backfillOnce(defaultBackfillDepth, 512)
		case <-n.stop:
			return
		}
	}
}

// backfillOnce reconciles this node's owned keyspace with peers by:
//  1. Computing local per-bucket digests for owned keys.
//  2. Asking each donor for its digests targeted at this node.
//  3. For buckets that differ, paging through donor keys in hash order
//     using a cursor, decoding values, and importing successfully decoded
//     items into the local shard (and LWW version table when enabled).
//
// The donor list excludes self and peers we are not connected to.
func (n *Node[K, V]) backfillOnce(depth int, page int) {
	donors := n.peerIDs()
	selfID := n.cfg.ID
	tmp := donors[:0]
	for _, d := range donors {
		if d != selfID && n.getPeer(d) != nil {
			tmp = append(tmp, d)
		}
	}

	donors = tmp
	if len(donors) == 0 {
		return
	}

	// Compute a local view of bucket digests we own to detect divergence.
	local := n.computeLocalDigests(depth)
	sort.Slice(donors, func(i, j int) bool { return donors[i] < donors[j] })

	for _, d := range donors {
		pc := n.getPeer(d)
		if pc == nil {
			continue
		}

		req := &MsgBackfillDigestReq{
			Base:     Base{T: MTBackfillDigestReq, ID: n.nextReqID()},
			TargetID: string(selfID),
			Depth:    uint8(depth),
		}

		raw, err := pc.request(req, req.ID, n.cfg.Sec.ReadTimeout)
		if err != nil {
			continue
		}

		var dr MsgBackfillDigestResp
		if e := cbor.Unmarshal(raw, &dr); e != nil {
			continue
		}
		if dr.NotInRing {
			// Donor hasn't integrated us into its ring yet; skip this donor for now
			continue
		}
		if len(dr.Buckets) == 0 {
			continue
		}

		for _, b := range dr.Buckets {
			lp := local[string(b.Prefix)]
			if lp.count == b.Count && lp.hash == b.Hash64 {
				continue // already in sync for this bucket
			}

			// Page through differing buckets using last key-hash cursor to keep
			// pagination deterministic and avoid duplicates/skips across pages.
			var cursor []byte
			for {
				kReq := &MsgBackfillKeysReq{
					Base:     Base{T: MTBackfillKeysReq, ID: n.nextReqID()},
					TargetID: string(selfID),
					Prefix:   append([]byte(nil), b.Prefix...),
					Limit:    page,
					Cursor:   cursor,
				}

				raw2, err := pc.request(kReq, kReq.ID, n.cfg.Sec.ReadTimeout)
				if err != nil {
					break
				}

				var kr MsgBackfillKeysResp
				if e := cbor.Unmarshal(raw2, &kr); e != nil {
					break
				}
				if kr.NotInRing {
					// Donor not ready; stop paging this bucket from this donor for now
					break
				}
				if len(kr.Items) == 0 {
					break
				}

				// Decode and import only keys that successfully decode and pass
				// size/time limits; errors are skipped to keep repair moving.
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
					// Update our running local digest with imported batch to avoid
					// asking for keys we've already reconciled in this pass.
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

// computeLocalDigests returns an orderless digest per key-hash prefix
// bucket for keys this node currently owns. The digest includes count and
// XOR(hash^version) so that donors and joiners can cheaply detect drift
// without moving all keys. Depth is clamped to [1,8] bytes of the 64-bit
// key hash in big-endian order.
// helper (optional)
func (n *Node[K, V]) computeLocalDigests(depth int) map[string]bucketSig {
	if depth <= 0 || depth > 8 {
		depth = 2
	}

	m := make(map[string]bucketSig, 1<<12)
	keys := n.local.Keys()
	r := n.ring.Load().(*ring)
	selfID := n.cfg.ID

	for _, k := range keys {
		h64 := n.hash64Of(k)
		if !r.ownsHash(selfID, h64) {
			continue
		}

		var hb [8]byte
		binary.BigEndian.PutUint64(hb[:], h64)
		prefix := string(hb[:depth])

		ver := uint64(0)
		if n.cfg.LWWEnabled {
			kb := n.kc.EncodeKey(k)
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

// updateLocalDigestWithBatch updates an existing local digest with a set
// of imported items so subsequent comparisons consider already-synced
// keys and avoid re-requesting them in the same backfill run.
func (n *Node[K, V]) updateLocalDigestWithBatch(m map[string]bucketSig, depth int, batch []cache.Item[K, V]) map[string]bucketSig {
	for _, it := range batch {
		h64 := n.hash64Of(it.Key)
		var hb [8]byte
		binary.BigEndian.PutUint64(hb[:], h64)
		prefix := string(hb[:depth])

		ver := uint64(0)
		if n.cfg.LWWEnabled {
			ver = it.Version
		}

		s := m[prefix]
		s.count++
		s.hash ^= (h64 ^ ver)
		m[prefix] = s
	}
	return m
}

func (n *Node[K, V]) hash64Of(k K) uint64 {
	if kh, ok := any(n.kc).(KeyHasher[K]); ok {
		return kh.Hash64(k)
	}
	return xxhash.Sum64(n.kc.EncodeKey(k))
}
