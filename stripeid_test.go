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

	// Overflow IDs are consecutive and are not tracked.
	if id := a.acquire(); id != stripeIDCap {
		t.Fatalf("first overflow id = %d, want %d", id, stripeIDCap)
	}
	if id := a.acquire(); id != stripeIDCap+1 {
		t.Fatalf("second overflow id = %d, want %d", id, stripeIDCap+1)
	}
	a.release(stripeIDCap + 44)

	// Released IDs are reused from the lowest available value.
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
	// Keep tokens out of the pool so garbage collection returns their IDs.
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
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("lowest free id still %d after GC, want %d", id, before)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStripeIDMaskedStable(t *testing.T) {
	// Preemption or a pool clear can replace a token, so allow several attempts
	// to observe two calls using the same ID.
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
	// The pointer prevents the runtime's tiny allocator from delaying cleanup
	// and leaking IDs. Keep this check with stripeToken's matching comment.
	typ := reflect.TypeOf(stripeToken{})
	for i := range typ.NumField() {
		if typ.Field(i).Type.Kind() == reflect.Pointer {
			return
		}
	}
	t.Fatal("stripeToken has no pointer field; tiny-allocator batching can starve its cleanup")
}
