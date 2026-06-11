package kioshun

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

	// counters are grouped into cache-line blocks of 8 words (128 4-bit
	// counters). One access touches a single block - selected by the low bits
	// of the caller's avalanched fingerprint - with the four row offsets taken
	// from disjoint higher bit ranges so an add or estimate costs one hash and
	// one cache line instead of four of each.
	sketchBlockWords    = 8
	sketchBlockCounters = sketchBlockWords * sketchCountersPerWord
)

// doorkeeper is a 2-hash Bloom filter that absorbs first-time accesses so the
// count-min sketch only spends counters on keys seen at least twice. Both probe
// indexes derive from disjoint bit ranges of the caller's avalanched
// fingerprint, sharing the one hash the sketch already needs.
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

// countMinSketch estimates access frequency in 4-bit saturating counters packed
// 16 per word and grouped into cache-line blocks. Counters periodically halve
// (age) so the estimate tracks a sliding window of popularity rather than
// all-time totals.
type countMinSketch struct {
	counters  []uint64
	blockMask uint64 // block count - 1; counters holds blockCount*8 words
	samples   uint64
	resetAt   uint64
}

// newCountMinSketch rounds the logical counter count to a power of two so block
// selection can use a mask instead of modulo.
func newCountMinSketch(n uint64) countMinSketch {
	if n < sketchMinCounters {
		n = sketchMinCounters
	}
	n = uint64(nextPowerOf2(int(n)))

	w := n / sketchCountersPerWord
	return countMinSketch{
		counters:  make([]uint64, w),
		blockMask: w/sketchBlockWords - 1,
		resetAt:   n * sketchAgingMultiplier,
	}
}

// add increments all four rows only while the estimated frequency is below the
// saturation limit. Once the estimate reaches the 4-bit maximum, extra accesses
// are ignored until aging makes room again.
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

// indexes maps av to four counter cells inside one block. The block comes from
// the low bits; the in-block offsets start at bit 21, which stays clear of the
// block mask up to 2^21 blocks (256M counters - no per-shard sketch gets
// anywhere near that). Past it the first offset would share bits with block
// selection: a mild correlation, not an error. Two offsets can land on the
// same cell, costing one effective row, but that loss is small next to what
// the single cache line per access buys.
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
