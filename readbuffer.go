package kioshun

import (
	"runtime"
	"sync/atomic"

	"github.com/unkn0wn-root/kioshun/internal/mathx"
)

const (
	// readStripeSlots is how many access fingerprints a stripe buffers before
	// producers begin overwriting the oldest unread sample.
	// It also sets the drain-signal: one wake per filled stripe.
	// Note: must be 2^n.
	readStripeSlots = 64
	readSlotMask    = readStripeSlots - 1

	// caps per-shard striping. Shards already partition keys, so
	// matching common GOMAXPROCS values spreads hot shard readers without
	// allocating an unbounded number of per shard rings.
	// Must stay <= 32 so one uint32 can hold a dirty bit per stripe.
	maxReadStripes = 16
)

// readStripe is a "lossy" multi-producer/single-consumer ring of access
// fingerprints (key hashes). Readers append wait-free; the shard's write worker
// is the only consumer. When producers outrun the consumer the oldest samples
// are overwritten - acceptable because samples only feed the frequency sketch,
// where a dropped sample costs a little accuracy but never correctness.
type readStripe struct {
	tail atomic.Uint64
	head atomic.Uint64
	buf  [readStripeSlots]atomic.Uint64
}

// readBuffer is the per-shard BP-Wrapper read buffer: a small set of striped
// rings indexed by a per-P stripe id to reduce contention on hot shards. The
// zero value is unused (no stripes); only SieveTinyLFU shards allocate one.
type readBuffer struct {
	stripes []readStripe
	mask    uint64

	// dirty has one bit per stripe: producers set it on a stripe's first sample, the
	// consumer clears it only when the stripe turns out quiet. The write path's
	// constant "any samples pending?" check is then one word, not a walk over every
	// stripe's cursors, and a busy stripe's bit just stays set (no steady-state
	// read-modify-write). Lossy: a sample racing the clear is delayed to the next
	// sample, not lost.
	dirty atomic.Uint32
}

func newReadBuffer() readBuffer {
	n := max(mathx.NextPowerOf2(min(runtime.GOMAXPROCS(0), maxReadStripes)), 1)
	return readBuffer{
		stripes: make([]readStripe, n),
		mask:    uint64(n - 1),
	}
}

// sample records an access fingerprint into the stripe picked by the caller's id
// and returns that stripe's index plus whether the consumer has fallen a full window
// behind. Past readStripeSlots of backlog, producers overwrite unread samples, so
// the caller drains that stripe or wakes the worker. The head load is a hint (head
// only advances): a stale read may over-report backlog and cause an extra drain, but
// cannot hide a full window. Lossy by design.
func (rb *readBuffer) sample(h, id uint64) (stripe int, needDrain bool) {
	if h == 0 {
		h = 1
	}
	idx := id & rb.mask
	st := &rb.stripes[idx]
	i := st.tail.Add(1) - 1
	st.buf[i&readSlotMask].Store(h)
	if bit := uint32(1) << idx; rb.dirty.Load()&bit == 0 {
		rb.dirty.Or(bit)
	}
	return int(idx), (i+1)-st.head.Load() >= readStripeSlots
}
