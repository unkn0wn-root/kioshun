package kioshun

import (
	"runtime"
	"sync/atomic"

	"github.com/unkn0wn-root/kioshun/internal/mathutil"
)

const (
	// readStripeSlots is how many access fingerprints a stripe buffers before
	// producers begin overwriting the oldest unread sample.
	// It also sets the drain-signal cadence: one wake per filled stripe.
	// Note: must be 2^n.
	readStripeSlots = 64
	readSlotMask    = readStripeSlots - 1

	// maxReadStripes caps per-shard striping. Shards already partition keys so
	// matching common GOMAXPROCS values spreads hot-shard readers without
	// allocating an unbounded number of per-shard rings.
	maxReadStripes = 16
)

// readStripe is a "lossy" multi-producer/single-consumer ring of access
// fingerprints (key hashes). Readers append wait-free; the shard's write worker
// is the only consumer. When producers outrun the consumer the oldest samples
// are overwritten — acceptable because samples only feed the frequency sketch,
// where a dropped sample costs a little accuracy but never correctness.
type readStripe struct {
	tail atomic.Uint64                  // next write index (producers)
	head uint64                         // next read index (consumer)
	buf  [readStripeSlots]atomic.Uint64 // fingerprints; 0 == empty slot
}

// readBuffer is the per-shard BP-Wrapper read buffer: a small set of striped
// rings indexed by the producer's P id to reduce contention on hot shards. The
// zero value is unused (no stripes); only SieveTinyLFU shards allocate one.
type readBuffer struct {
	stripes []readStripe
	mask    uint64
}

// newReadBuffer sizes the stripe set to GOMAXPROCS (capped)
// rounded to a 2^n for mask-based indexing.
func newReadBuffer() readBuffer {
	n := max(mathutil.NextPowerOf2(min(runtime.GOMAXPROCS(0), maxReadStripes)), 1)
	return readBuffer{
		stripes: make([]readStripe, n),
		mask:    uint64(n - 1),
	}
}

// sample records an access fingerprint into the caller's P-local stripe and
// reports whether that stripe just filled, so the caller can wake the drain
// (coalesced). Lossy by design.
func (rb *readBuffer) sample(h uint64) bool {
	if h == 0 {
		h = 1 // reserve 0 as the empty slot sentinel
	}
	st := &rb.stripes[uint64(procID())&rb.mask]
	i := st.tail.Add(1) - 1
	st.buf[i&readSlotMask].Store(h)
	return i&readSlotMask == readSlotMask
}
