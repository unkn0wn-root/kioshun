package kioshun

import (
	"reflect"
	"runtime"
	"testing"
	"time"
)

func TestStripeIDAllocDistinctAndReuse(t *testing.T) {
	var a stripeIDAlloc

	seen := make(map[uint64]bool)
	for range stripeIDCap {
		id := a.acquire()
		if id >= stripeIDCap {
			t.Fatalf("id %d out of bitmap range before exhaustion", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %d", id)
		}
		seen[id] = true
	}

	// exhausted: overflow ids are consecutive and not tracked
	if id := a.acquire(); id != stripeIDCap {
		t.Fatalf("first overflow id = %d, want %d", id, stripeIDCap)
	}
	if id := a.acquire(); id != stripeIDCap+1 {
		t.Fatalf("second overflow id = %d, want %d", id, stripeIDCap+1)
	}
	a.release(stripeIDCap + 44) // overflow release is a no-op, must not corrupt the bitmap

	// released ids come back lowest-first
	a.release(7)
	a.release(3)
	if id := a.acquire(); id != 3 {
		t.Fatalf("acquire after release = %d, want 3", id)
	}
	if id := a.acquire(); id != 7 {
		t.Fatalf("acquire after release = %d, want 7", id)
	}
}

func TestStripeTokenCleanupReleasesID(t *testing.T) {
	// Mint tokens without parking them in the pool, so the GC can collect
	// them and the cleanup has to hand their ids back.
	before := stripeIDs.acquire()
	stripeIDs.release(before)

	for range 8 {
		_ = newStripeToken()
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		runtime.GC()
		id := stripeIDs.acquire()
		stripeIDs.release(id)
		if id <= before {
			return // the issued ids were handed back
		}
		if time.Now().After(deadline) {
			t.Fatalf("lowest free id still %d after GC, want %d", id, before)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStripeIDMaskedStable(t *testing.T) {
	// The pool hands the parked token back, so back-to-back calls on one
	// goroutine return one id. Preemption or a GC pool clear between two
	// calls can legitimately swap the token, so require a stable pair within
	// a few attempts rather than on the first.
	const mask = maxReadStripes - 1
	for range 100 {
		first := stripeID() & mask
		second := stripeID() & mask
		if first == second {
			return
		}
	}
	t.Fatal("id never sticky across 100 back-to-back call pairs")
}

func TestStripeTokenNotTinyBatched(t *testing.T) {
	// Id reuse rides on runtime.AddCleanup, and the cleanup of a tiny
	// pointer-free object may never run when the runtime batches it into a
	// shared allocation block with something longer-lived. A pointer field
	// keeps the token off the tiny-allocator path entirely; do not "simplify"
	// it away or ids leak.
	typ := reflect.TypeOf(stripeToken{})
	for i := range typ.NumField() {
		if typ.Field(i).Type.Kind() == reflect.Pointer {
			return
		}
	}
	t.Fatal("stripeToken has no pointer field; tiny-allocator batching can starve its cleanup")
}
