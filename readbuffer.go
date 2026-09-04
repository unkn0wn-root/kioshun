package kioshun

import (
	"runtime"
	"sync/atomic"

	"github.com/unkn0wn-root/kioshun/internal/mathx"
)

const (
	// readStripeSlots must be a power of two.
	readStripeSlots = 64
	readSlotMask    = readStripeSlots - 1

	// maxReadStripes must be at most 32 because dirty uses one bit per stripe.
	maxReadStripes = 16
)

// readStripe is a lossy ring of key hashes with many producers and one consumer.
// Readers never wait. If they overtake the consumer, old samples are discarded;
// this affects frequency estimates but not cache correctness.
type readStripe struct {
	tail atomic.Uint64
	head atomic.Uint64
	buf  [readStripeSlots]atomic.Uint64
}

// readBuffer spreads a shard's read samples across rings to reduce contention.
type readBuffer struct {
	stripes []readStripe
	mask    uint64

	// dirty lets the consumer find rings with pending samples in one load. A
	// producer sets its ring's bit; the consumer clears it only when the ring is
	// empty. A sample that races the clear is picked up after the next sample.
	dirty atomic.Uint32
}

func newReadBuffer() readBuffer {
	n := max(mathx.NextPowerOf2(min(runtime.GOMAXPROCS(0), maxReadStripes)), 1)
	return readBuffer{
		stripes: make([]readStripe, n),
		mask:    uint64(n - 1),
	}
}

// sample stores a key hash and reports when the consumer is a full ring behind.
// Its head read may be stale, which can request an unnecessary drain but cannot
// hide a full ring.
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
