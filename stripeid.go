package kioshun

import (
	"math/bits"
	"runtime"
	"sync"
)

// stripeIDCap is how many ids the allocator bitmap tracks, far above any realistic GOMAXPROCS.
const (
	stripeIDWords = 4
	stripeIDCap   = stripeIDWords * 64
)

// stripeIDs hands out the lowest free index and reclaims it when the owning token
// is GCed, so the indices held by live tokens stay distinct. Random indices would
// not: with about as many producers as stripes, some would collide on a stripe and
// bounce its cache line between cores.
var stripeIDs stripeIDAlloc

type stripeIDAlloc struct {
	mu       sync.Mutex
	used     [stripeIDWords]uint64 // bitmap of live ids 0..stripeIDCap-1
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
	// All tracked ids are taken; hand out consecutive values instead.
	// They still spread fine over any power-of-two stripe count.
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

// stripeTokens holds roughly one stripeToken per P: sync.Pool's private slot returns
// the token last released on the current P, so repeated calls on the same P reuse
// the same id and the stripe choice follows the P. One process-wide pool suffices -
// the id only spreads producers across stripes. New tokens issue rarely (startup, or
// after an idle P's token was collected), keeping the allocator mutex and cleanup
// registration off the hot path.
var stripeTokens = sync.Pool{New: newStripeToken}

// stripeToken has a pointer field for one reason: it keeps the token off the
// runtime's tiny allocator which packs small pointer free objects into a
// shared memory block. As long as anything in that block is still alive, the
// dead objects in it are not collected and their cleanups may never run - for
// us that would mean leaked token ids. Objects containing pointers are never
// packed like this, no matter their size.
type stripeToken struct {
	idx uint64
	_   *byte
}

func newStripeToken() any {
	t := &stripeToken{idx: stripeIDs.acquire()}
	runtime.AddCleanup(t, stripeIDs.release, t.idx)
	return t
}

// stripeID returns the caller's index into the striped structures (read-sample
// rings, stat counters). Best-effort: a goroutine preempted between Get and Put, or
// a GOMAXPROCS change, can leave two producers on the same stripe briefly. Every
// striped consumer tolerates that, and callers mask the value so it cannot index out
// of range.
func stripeID() uint64 {
	t := stripeTokens.Get().(*stripeToken)
	idx := t.idx
	stripeTokens.Put(t)
	return idx
}
