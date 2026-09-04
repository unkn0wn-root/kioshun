package kioshun

import (
	"math/bits"
	"runtime"
	"sync"
)

const (
	stripeIDWords = 4
	stripeIDCap   = stripeIDWords * 64
)

// stripeIDs keeps live tokens on different stripes when possible. Tokens return
// their IDs when the garbage collector reclaims them.
var stripeIDs stripeIDAlloc

type stripeIDAlloc struct {
	mu       sync.Mutex
	used     [stripeIDWords]uint64
	overflow uint64
}

func (a *stripeIDAlloc) acquire() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	for w := range a.used {
		if free := ^a.used[w]; free != 0 {
			b := bits.TrailingZeros64(free)
			a.used[w] |= 1 << b
			return uint64(w<<6 | b)
		}
	}
	// Consecutive overflow IDs still spread across a power-of-two stripe count.
	id := stripeIDCap + a.overflow
	a.overflow++
	return id
}

func (a *stripeIDAlloc) release(id uint64) {
	if id >= stripeIDCap {
		return
	}
	a.mu.Lock()
	a.used[id>>6] &^= 1 << (id & 63)
	a.mu.Unlock()
}

// sync.Pool usually returns the token last used on the current P. Reusing its ID
// keeps work on the same stripe and avoids frequent allocation.
var stripeTokens = sync.Pool{New: newStripeToken}

// The pointer keeps stripeToken out of the runtime's tiny allocator. Tiny
// pointer-free objects can share a block whose cleanup is delayed by another
// live object, which would keep stripe IDs allocated indefinitely.
type stripeToken struct {
	idx uint64
	_   *byte
}

func newStripeToken() any {
	t := &stripeToken{idx: stripeIDs.acquire()}
	runtime.AddCleanup(t, stripeIDs.release, t.idx)
	return t
}

// stripeID returns an index for striped read buffers and counters. The mapping is
// best effort: preemption or a GOMAXPROCS change can briefly share an index.
func stripeID() uint64 {
	t := stripeTokens.Get().(*stripeToken)
	idx := t.idx
	stripeTokens.Put(t)
	return idx
}
