package kioshun

import "math/bits"

const (
	sketchMinCounters     = 1024
	sketchAgingMultiplier = 10
	sketchCountersPerWord = 16
	sketchCounterBits     = 4
	sketchCounterMask     = 0x0f
	sketchMaxCounter      = 15
	// aging shifts packed 4-bit counters right by one; this mask clears bits
	// that would bleed. Keep tied to sketchCounterBits.
	sketchCounterAgingMask = 0x7777777777777777
	sketchHashRotation0    = 0
	sketchHashRotation1    = 17
	sketchHashRotation2    = 31
	sketchHashRotation3    = 47
)

// doorkeeper is a 2-hash Bloom filter that absorbs first-time accesses so the
// count-min sketch only spends counters on keys seen at least twice.
type doorkeeper struct {
	bits []uint64
	mask uint64
}

func newDoorkeeper(n uint64) doorkeeper {
	if n < 64 {
		n = 64
	}
	n = uint64(nextPowerOf2(int(n)))
	return doorkeeper{
		bits: make([]uint64, n/64),
		mask: n - 1,
	}
}

// add sets the doorkeeper bits and reports whether the fingerprint was already
// present. Callers use the return value to keep first-time accesses out of the
// heavier count-min sketch.
func (d *doorkeeper) add(h uint64) bool {
	if len(d.bits) == 0 {
		return true
	}

	i, j := d.indexes(h)
	exists := d.has(i) && d.has(j)
	d.set(i)
	d.set(j)
	return exists
}

func (d *doorkeeper) contains(h uint64) bool {
	if len(d.bits) == 0 {
		return false
	}

	i, j := d.indexes(h)
	return d.has(i) && d.has(j)
}

func (d *doorkeeper) clear() {
	clear(d.bits)
}

func (d *doorkeeper) indexes(h uint64) (uint64, uint64) {
	return sketchIndex(h, d.mask, sketchHashRotation1), sketchIndex(h, d.mask, sketchHashRotation3)
}

func (d *doorkeeper) has(i uint64) bool {
	return d.bits[i/64]&(uint64(1)<<(i%64)) != 0
}

func (d *doorkeeper) set(i uint64) {
	d.bits[i/64] |= uint64(1) << (i % 64)
}

// countMinSketch estimates access frequency in 4-bit saturating counters packed
// 16 per word. Counters periodically halve (age) so the estimate tracks a
// sliding window of popularity rather than all-time totals.
type countMinSketch struct {
	counters []uint64
	mask     uint64
	samples  uint64
	resetAt  uint64
}

// newCountMinSketch rounds the logical counter count to a power of two so hash
// indexes can use a mask instead of modulo.
func newCountMinSketch(n uint64) countMinSketch {
	if n < sketchMinCounters {
		n = sketchMinCounters
	}
	n = uint64(nextPowerOf2(int(n)))

	w := n / sketchCountersPerWord
	if w == 0 {
		w = 1
	}

	return countMinSketch{
		counters: make([]uint64, w),
		mask:     n - 1,
		resetAt:  n * sketchAgingMultiplier,
	}
}

func (s *countMinSketch) increment(h uint64) {
	s.add(h)
	s.samples++
	if s.resetAt > 0 && s.samples >= s.resetAt {
		s.age()
	}
}

// add increments all four rows only while the estimated frequency is below the
// saturation limit. Once the estimate reaches the 4-bit maximum, extra accesses
// are ignored until aging makes room again.
func (s *countMinSketch) add(h uint64) {
	if len(s.counters) == 0 {
		return
	}

	idx := s.indexes(h)
	if s.minCounter(idx) < sketchMaxCounter {
		for _, i := range idx {
			s.incrementCounter(i)
		}
	}
}

func (s *countMinSketch) estimate(h uint64) uint8 {
	if len(s.counters) == 0 {
		return 0
	}

	return s.minCounter(s.indexes(h))
}

func (s *countMinSketch) indexes(h uint64) [4]uint64 {
	return [4]uint64{
		sketchIndex(h, s.mask, sketchHashRotation0),
		sketchIndex(h, s.mask, sketchHashRotation1),
		sketchIndex(h, s.mask, sketchHashRotation2),
		sketchIndex(h, s.mask, sketchHashRotation3),
	}
}

func (s *countMinSketch) minCounter(idx [4]uint64) uint8 {
	min := s.counter(idx[0])
	for _, i := range idx[1:] {
		if v := s.counter(i); v < min {
			min = v
		}
	}

	return min
}

// age halves every packed 4-bit counter in place. The mask removes shifted bits
// that would otherwise leak from one counter nibble into the next.
func (s *countMinSketch) age() {
	for i := range s.counters {
		s.counters[i] = (s.counters[i] >> 1) & sketchCounterAgingMask
	}
	s.samples = 0
}

func (s *countMinSketch) clear() {
	clear(s.counters)
	s.samples = 0
}

func (s *countMinSketch) counter(i uint64) uint8 {
	wi := i / sketchCountersPerWord
	ci := i % sketchCountersPerWord
	sh := ci * sketchCounterBits
	return uint8((s.counters[wi] >> sh) & sketchCounterMask)
}

func (s *countMinSketch) incrementCounter(i uint64) {
	wi := i / sketchCountersPerWord
	ci := i % sketchCountersPerWord
	sh := ci * sketchCounterBits
	v := (s.counters[wi] >> sh) & sketchCounterMask
	if v < sketchMaxCounter {
		s.counters[wi] += 1 << sh
	}
}

// sketchIndex derives one sketch row index from a shared 64-bit hash. The
// caller supplies a rotation to decorrelate rows; m must be a power-of-two mask.
func sketchIndex(h, m uint64, rot int) uint64 {
	h = bits.RotateLeft64(h, rot)
	return xxHash64Avalanche(h) & m
}
