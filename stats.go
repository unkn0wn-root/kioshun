package kioshun

import "sync/atomic"

const statStripeCap = 16

type statStripe struct {
	hits        atomic.Int64
	misses      atomic.Int64
	evictions   atomic.Int64
	expirations atomic.Int64
	_           [cacheLinePadding]byte
}

type stats struct {
	stripes []statStripe
	mask    uint64
}

func newStats(parallelism int) *stats {
	n := max(nextPowerOf2(min(parallelism, statStripeCap)), 1)
	return &stats{stripes: make([]statStripe, n), mask: uint64(n - 1)}
}

func (s *stats) stripe() *statStripe {
	return &s.stripes[uint64(procID())&s.mask]
}

func (s *stats) recordHit()        { s.stripe().hits.Add(1) }
func (s *stats) recordMiss()       { s.stripe().misses.Add(1) }
func (s *stats) recordEviction()   { s.stripe().evictions.Add(1) }
func (s *stats) recordExpiration() { s.stripe().expirations.Add(1) }

func (s *stats) aggregate() (hits, misses, evictions, expirations int64) {
	for i := range s.stripes {
		hits += s.stripes[i].hits.Load()
		misses += s.stripes[i].misses.Load()
		evictions += s.stripes[i].evictions.Load()
		expirations += s.stripes[i].expirations.Load()
	}
	return
}
