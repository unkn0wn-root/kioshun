package keyhash

import (
	"hash/maphash"
	"reflect"
	"unsafe"
)

// Hash finalization shared by int key hashing, ghost probing and sketch indexing.
const (
	prime64_2 = 0xC2B2AE3D27D4EB4F
	prime64_3 = 0x165667B19E3779F9

	// xor-shifts for the avalanche finalizer (order matters).
	avalancheShift1 = 33
	avalancheShift2 = 29
	avalancheShift3 = 32
)

// hashableInteger is every key kind we hash by reading its integer
// representation directly. New selects T from reflect.Kind so the unsafe read
// matches K's integer representation.
type hashableInteger interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 | ~uintptr
}

// Hasher carries its own seed, like the runtime gives every map its own so
// a key set that hashes badly into one cache does not hash badly into all of
// them. Hashes stay stable for the Hasher's lifetime meaning that all the cache needs,
// they are never persisted - and differ between Hashers and between runs.
// Integer keys are the exception: they skip the seed and only get the
// avalanche mix, which is cheaper.
type Hasher[K comparable] struct {
	seed maphash.Seed
	sum  func(maphash.Seed, K) uint64
}

func New[K comparable]() Hasher[K] {
	h := Hasher[K]{seed: maphash.MakeSeed()}
	switch reflect.TypeFor[K]().Kind() {
	case reflect.String:
		h.sum = hashStringKey[K]
	case reflect.Int:
		h.sum = hashIntKey[K, int]
	case reflect.Int8:
		h.sum = hashIntKey[K, int8]
	case reflect.Int16:
		h.sum = hashIntKey[K, int16]
	case reflect.Int32:
		h.sum = hashIntKey[K, int32]
	case reflect.Int64:
		h.sum = hashIntKey[K, int64]
	case reflect.Uint:
		h.sum = hashIntKey[K, uint]
	case reflect.Uint8:
		h.sum = hashIntKey[K, uint8]
	case reflect.Uint16:
		h.sum = hashIntKey[K, uint16]
	case reflect.Uint32:
		h.sum = hashIntKey[K, uint32]
	case reflect.Uint64:
		h.sum = hashIntKey[K, uint64]
	case reflect.Uintptr:
		h.sum = hashIntKey[K, uintptr]
	default:
		h.sum = hashFallbackKey[K]
	}
	return h
}

func (h Hasher[K]) Sum(key K) uint64 {
	return h.sum(h.seed, key)
}

// hashStringKey reinterprets K (known from reflect.Kind to be a string kind) as
// a string without copying. This keeps named string types on the same hot path
// as plain string without reflection or allocation per hash.
func hashStringKey[K comparable](seed maphash.Seed, key K) uint64 {
	return maphash.String(seed, *(*string)(unsafe.Pointer(&key)))
}

func hashIntKey[K comparable, T hashableInteger](_ maphash.Seed, key K) uint64 {
	return Avalanche(uint64(*(*T)(unsafe.Pointer(&key))))
}

// hashFallbackKey covers every remaining comparable kind (structs, arrays,
// pointers, floats, channels, interfaces) by hashing the value's memory
// representation, the same way the built-in map does.
func hashFallbackKey[K comparable](seed maphash.Seed, key K) uint64 {
	return maphash.Comparable(seed, key)
}

// Avalanche runs the final xor-shift/multiply chain that enforces avalanche
// behavior. It finalizes int-key hashes, ghost tags and sketch indexes.
func Avalanche(h64 uint64) uint64 {
	h64 ^= h64 >> avalancheShift1
	h64 *= prime64_2
	h64 ^= h64 >> avalancheShift2
	h64 *= prime64_3
	h64 ^= h64 >> avalancheShift3
	return h64
}
