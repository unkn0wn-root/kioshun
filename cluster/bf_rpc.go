package cluster

import (
	"bytes"
	"encoding/binary"
	"sort"
	"time"

	"github.com/cespare/xxhash/v2"
)

// hasTargetOwner reports whether target appears among owners (by Addr).
func hasTargetOwner(owners []*nodeMeta, target string) bool {
	for _, o := range owners {
		if o.Addr == target {
			return true
		}
	}
	return false
}

func absExpiryAt(base time.Time, ttl time.Duration) int64 {
	if ttl <= 0 {
		return 0
	}
	return base.Add(ttl).UnixNano()
}

// rpcBackfillDigest builds digests for the requested prefix depth considering
// only keys that the target node should own (according to this donor's ring).
// It returns per-bucket counts and XOR(hash^version) so the joiner can detect
// which buckets differ and page only those keys.
func (n *Node[K, V]) rpcBackfillDigest(req MsgBackfillDigestReq) MsgBackfillDigestResp {
	depth := int(req.Depth)
	if depth <= 0 || depth > 8 {
		depth = 2
	}
	target := req.Target
	r := n.ring.Load().(*ring)

	type agg struct {
		c uint32
		h uint64
	}

	// Aggregate per-bucket count and XOR(hash^version) to detect
	// differences between donor and joiner without shipping all keys.
	buckets := make(map[string]agg, 1<<12)

	keys := n.local.Keys()
	for _, k := range keys {
		kb := n.kc.EncodeKey(k)

		var h64 uint64
		if kh, ok := any(n.kc).(KeyHasher[K]); ok {
			h64 = kh.Hash64(k)
		} else {
			h64 = xxhash.Sum64(kb)
		}

		owners := r.ownersFromKeyHash(h64)
		if !hasTargetOwner(owners, target) {
			continue
		}

		var ver uint64
		if n.cfg.LWWEnabled {
			n.verMu.RLock()
			ver = n.version[string(kb)]
			n.verMu.RUnlock()
		}

		var hb [8]byte
		binary.BigEndian.PutUint64(hb[:], h64)
		prefix := string(hb[:depth])

		a := buckets[prefix]
		a.c++
		a.h ^= (h64 ^ ver)
		buckets[prefix] = a
	}

	out := make([]BucketDigest, 0, len(buckets))
	for p, a := range buckets {
		out = append(out, BucketDigest{Prefix: []byte(p), Count: a.c, Hash64: a.h})
	}
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i].Prefix, out[j].Prefix) < 0 })
	return MsgBackfillDigestResp{
		Base:    Base{T: MTBackfillDigestResp, ID: req.ID},
		Depth:   uint8(depth),
		Buckets: out,
	}
}

// rpcBackfillKeys returns the next page of keys within a given hash-prefix
// bucket that the target should own, ordered by 64-bit key hash. Pagination is
// driven by the last 8-byte hash cursor provided by the caller. Values may be
// compressed, and expirations are converted to absolute nanoseconds.
func (n *Node[K, V]) rpcBackfillKeys(req MsgBackfillKeysReq) MsgBackfillKeysResp {
	prefix := req.Prefix
	depth := len(prefix)
	if depth <= 0 || depth > 8 {
		return MsgBackfillKeysResp{Base: Base{T: MTBackfillKeysResp, ID: req.ID}, Done: true}
	}

	target := req.Target
	limit := req.Limit
	if limit <= 0 || limit > 4096 {
		limit = 1024
	}

	// decode cursor (last key-hash). The donor walks keys by hash order
	// inside a bucket to provide consistent pagination.
	var after uint64
	if len(req.Cursor) == 8 {
		after = binary.BigEndian.Uint64(req.Cursor)
	}

	type row struct {
		h  uint64
		k  K
		kb []byte
	}
	rows := make([]row, 0, limit*2)

	r := n.ring.Load().(*ring)
	keys := n.local.Keys()
	for _, k := range keys {
		kb := n.kc.EncodeKey(k)

		var h64 uint64
		if kh, ok := any(n.kc).(KeyHasher[K]); ok {
			h64 = kh.Hash64(k)
		} else {
			h64 = xxhash.Sum64(kb)
		}

		var hb [8]byte
		binary.BigEndian.PutUint64(hb[:], h64)
		if string(hb[:depth]) != string(prefix) {
			continue
		}

		owners := r.ownersFromKeyHash(h64)
		if !hasTargetOwner(owners, target) || h64 <= after {
			continue
		}
		rows = append(rows, row{h: h64, k: k, kb: kb})
	}

	// sort by key-hash to respect the cursor pagination.
	sort.Slice(rows, func(i, j int) bool { return rows[i].h < rows[j].h })
	if len(rows) > limit {
		rows = rows[:limit]
	}

	items := make([]KV, 0, len(rows))
	now := time.Now()
	for _, r := range rows {
		v, ttl, ok := n.local.GetWithTTL(r.k)
		if !ok {
			continue
		}

		bv, _ := n.codec.Encode(v)
		b2, cp := n.maybeCompress(bv)

		var ver uint64
		if n.cfg.LWWEnabled {
			n.verMu.RLock()
			ver = n.version[string(r.kb)]
			n.verMu.RUnlock()
		}

		// 0 means no expiration.
		abs := absExpiryAt(now, ttl)
		items = append(items, KV{
			K:   append([]byte(nil), r.kb...),
			V:   append([]byte(nil), b2...),
			E:   abs,
			Ver: ver,
			Cp:  cp,
		})
	}

	resp := MsgBackfillKeysResp{
		Base:  Base{T: MTBackfillKeysResp, ID: req.ID},
		Items: items,
		Done:  len(items) == 0,
	}
	if len(rows) > 0 {
		var next [8]byte
		binary.BigEndian.PutUint64(next[:], rows[len(rows)-1].h)
		resp.NextCursor = next[:]
	}
	return resp
}
