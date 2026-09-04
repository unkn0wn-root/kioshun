package kioshun

import "github.com/unkn0wn-root/kioshun/internal/mathx"

const (
	sketchMinCounters     = 1024
	sketchAgingMultiplier = 10
	sketchCountersPerWord = 16
	sketchCounterBits     = 4
	sketchCounterMask     = (1 << sketchCounterBits) - 1
	sketchMaxCounter      = sketchCounterMask
	// The mask clears bits that cross counter boundaries during aging.
	sketchCounterAgingMask = (^uint64(0) / sketchCounterMask) * (sketchCounterMask >> 1)

	// Each estimate uses four counters in one cache-line-sized block. One mixed
	// hash selects both the block and the four offsets.
	sketchBlockWords    = 8
	sketchBlockCounters = sketchBlockWords * sketchCountersPerWord
)

// doorkeeper is a two-hash Bloom filter. It keeps first accesses out of the
// count-min sketch, saving its counters for keys seen more than once.
type doorkeeper struct {
	bits []uint64
	mask uint64
}

func newDoorkeeper(n uint64) doorkeeper {
	if n < 64 {
		n = 64
	}
	n = uint64(mathx.NextPowerOf2(int(n)))
	return doorkeeper{
		bits: make([]uint64, n/64),
		mask: n - 1,
	}
}

// add records a hash and reports whether both bits were already set.
func (d *doorkeeper) add(av uint64) bool {
	if len(d.bits) == 0 {
		return true
	}

	i, j := d.indexes(av)
	exists := d.has(i) && d.has(j)
	d.set(i)
	d.set(j)
	return exists
}

func (d *doorkeeper) contains(av uint64) bool {
	if len(d.bits) == 0 {
		return false
	}

	i, j := d.indexes(av)
	return d.has(i) && d.has(j)
}

func (d *doorkeeper) clear() {
	clear(d.bits)
}

func (d *doorkeeper) indexes(av uint64) (uint64, uint64) {
	return av & d.mask, (av >> 32) & d.mask
}

func (d *doorkeeper) has(i uint64) bool {
	return d.bits[i/64]&(uint64(1)<<(i%64)) != 0
}

func (d *doorkeeper) set(i uint64) {
	d.bits[i/64] |= uint64(1) << (i % 64)
}

// countMinSketch estimates access frequency with packed 4-bit counters. Aging
// periodically halves them so old activity fades.
type countMinSketch struct {
	counters  []uint64
	blockMask uint64
	samples   uint64
	resetAt   uint64
}

func newCountMinSketch(n uint64) countMinSketch {
	if n < sketchMinCounters {
		n = sketchMinCounters
	}
	n = uint64(mathx.NextPowerOf2(int(n)))

	w := n / sketchCountersPerWord
	return countMinSketch{
		counters:  make([]uint64, w),
		blockMask: w/sketchBlockWords - 1,
		resetAt:   n * sketchAgingMultiplier,
	}
}

// add increments all four counters until the estimate reaches its 4-bit limit.
func (s *countMinSketch) add(av uint64) {
	if len(s.counters) == 0 {
		return
	}

	idx := s.indexes(av)
	if s.minCounter(idx) < sketchMaxCounter {
		for _, i := range idx {
			s.incrementCounter(i)
		}
	}
}

func (s *countMinSketch) estimate(av uint64) uint8 {
	if len(s.counters) == 0 {
		return 0
	}

	return s.minCounter(s.indexes(av))
}

// indexes maps a mixed hash to four counters in one block. Block and offset bits
// do not overlap for any practical per-shard sketch size.
func (s *countMinSketch) indexes(av uint64) [4]uint64 {
	base := (av & s.blockMask) * sketchBlockCounters
	return [4]uint64{
		base + (av>>21)&(sketchBlockCounters-1),
		base + (av>>28)&(sketchBlockCounters-1),
		base + (av>>35)&(sketchBlockCounters-1),
		base + (av>>42)&(sketchBlockCounters-1),
	}
}

func (s *countMinSketch) minCounter(idx [4]uint64) uint8 {
	return min(
		s.counter(idx[0]),
		s.counter(idx[1]),
		s.counter(idx[2]),
		s.counter(idx[3]),
	)
}

// age halves every counter without letting bits cross packed counter boundaries.
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

func locate(i uint64) (word, shift uint64) {
	return i / sketchCountersPerWord, (i % sketchCountersPerWord) * sketchCounterBits
}

func (s *countMinSketch) counter(i uint64) uint8 {
	wi, sh := locate(i)
	return uint8((s.counters[wi] >> sh) & sketchCounterMask)
}

func (s *countMinSketch) incrementCounter(i uint64) {
	wi, sh := locate(i)
	if (s.counters[wi]>>sh)&sketchCounterMask < sketchMaxCounter {
		s.counters[wi] += 1 << sh
	}
}
