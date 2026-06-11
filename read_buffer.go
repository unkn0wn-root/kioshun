package kioshun

import (
	"runtime"
	"sync/atomic"
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
	tail atomic.Uint64                  // next write index (producers)
	head atomic.Uint64                  // next read index (consumer-written, producer-read as a hint)
	buf  [readStripeSlots]atomic.Uint64 // fingerprints; 0 == empty slot
}

// readBuffer is the per-shard BP-Wrapper read buffer: a small set of striped
// rings indexed by the producer's P id to reduce contention on hot shards. The
// zero value is unused (no stripes); only SieveTinyLFU shards allocate one.
type readBuffer struct {
	stripes []readStripe
	mask    uint64

	// dirty has one bit per stripe: producers set it on a stripe's first
	// sample, the consumer clears it only when the stripe turns out to be
	// quiet. The write path asks "any samples pending?" constantly, and the
	// mask makes that one word instead of a walk over every stripe's cursors.
	// A busy stripe's bit just stays set, so neither side pays a
	// read-modify-write in steady state. Lossy like the stripes themselves: a
	// sample racing the clear is delayed until the stripe's next sample
	// re-arms the bit, not lost.
	dirty atomic.Uint32
}

func newReadBuffer() readBuffer {
	n := max(nextPowerOf2(min(runtime.GOMAXPROCS(0), maxReadStripes)), 1)
	return readBuffer{
		stripes: make([]readStripe, n),
		mask:    uint64(n - 1),
	}
}

// sample records an access fingerprint into the caller's P-local stripe and
// returns that stripe's index plus whether the consumer has fallen a full window
// behind. Once tail-head reaches readStripeSlots, the next producer writes begin
// overwriting unread samples; the caller can drain exactly that stripe or wake
// the worker. The head load is a hint: head only advances, so a stale read can
// over-report backlog and cause an extra drain, but it cannot hide a full window
// that existed before the sample. Lossy by design.
func (rb *readBuffer) sample(h uint64) (stripe int, needDrain bool) {
	if h == 0 {
		h = 1 // reserve 0 as the empty slot sentinel
	}
	idx := uint64(procID()) & rb.mask
	st := &rb.stripes[idx]
	i := st.tail.Add(1) - 1
	st.buf[i&readSlotMask].Store(h)
	if bit := uint32(1) << idx; rb.dirty.Load()&bit == 0 {
		rb.dirty.Or(bit)
	}
	return int(idx), (i+1)-st.head.Load() >= readStripeSlots
}
