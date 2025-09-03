package cluster

import "sync"

type bufPool struct {
	sizes       []int
	pools       []sync.Pool
	indexBySize map[int]int
}

// newBufPool creates fixed-size byte slice pools for a small set of common
// buffer sizes to reduce allocations on hot paths (framed I/O).
func newBufPool(sizes []int) *bufPool {
	bp := &bufPool{
		sizes:       sizes,
		pools:       make([]sync.Pool, len(sizes)),
		indexBySize: make(map[int]int, len(sizes)),
	}
	for i, sz := range sizes {
		size := sz
		bp.pools[i].New = func() any {
			b := make([]byte, size)
			return b
		}
		bp.indexBySize[sz] = i
	}
	return bp
}

// class returns the index of the first bucket that can hold n bytes.
func (bp *bufPool) class(n int) int {
	for i, sz := range bp.sizes {
		if n <= sz {
			return i
		}
	}
	return -1
}

// get returns a slice of length n from an appropriate bucket (or an exact
// allocation if n exceeds the largest bucket size).
func (bp *bufPool) get(n int) []byte {
	if i := bp.class(n); i >= 0 {
		b := bp.pools[i].Get().([]byte)
		// return a slice of length n, capacity bucket size.
		return b[:n]
	}
	// big frame (> largest bucket): allocate exact.
	return make([]byte, n)
}

// put returns a buffer to the matching bucket by capacity. non-pooled sizes
// are dropped on the floor to avoid unbounded pool growth.
func (bp *bufPool) put(b []byte) {
	if i, ok := bp.indexBySize[cap(b)]; ok {
		// restore to full capacity before putting back.
		b = b[:bp.sizes[i]]
		bp.pools[i].Put(b)
	}
}
