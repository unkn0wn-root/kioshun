package keyhash

import (
	"fmt"
	"reflect"
	"unsafe"
)

// Hash finalization shared by integer-key hashing, ghost probing and sketch
// indexing. The full xxHash64 string implementation and its remaining
// constants live in stringhash_purego.go, compiled only under the
// kioshun_purego build tag. The default build hashes string keys with the Go
// runtime's memhash.
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

type Hasher[K comparable] struct {
	sum func(K) uint64
}

func New[K comparable]() Hasher[K] {
	switch reflect.TypeFor[K]().Kind() {
	case reflect.String:
		return Hasher[K]{sum: hashStringKey[K]}
	case reflect.Int:
		return Hasher[K]{sum: hashIntKey[K, int]}
	case reflect.Int8:
		return Hasher[K]{sum: hashIntKey[K, int8]}
	case reflect.Int16:
		return Hasher[K]{sum: hashIntKey[K, int16]}
	case reflect.Int32:
		return Hasher[K]{sum: hashIntKey[K, int32]}
	case reflect.Int64:
		return Hasher[K]{sum: hashIntKey[K, int64]}
	case reflect.Uint:
		return Hasher[K]{sum: hashIntKey[K, uint]}
	case reflect.Uint8:
		return Hasher[K]{sum: hashIntKey[K, uint8]}
	case reflect.Uint16:
		return Hasher[K]{sum: hashIntKey[K, uint16]}
	case reflect.Uint32:
		return Hasher[K]{sum: hashIntKey[K, uint32]}
	case reflect.Uint64:
		return Hasher[K]{sum: hashIntKey[K, uint64]}
	case reflect.Uintptr:
		return Hasher[K]{sum: hashIntKey[K, uintptr]}
	default:
		return Hasher[K]{sum: hashFallbackKey[K]}
	}
}

func (h Hasher[K]) Sum(key K) uint64 {
	return h.sum(key)
}

// hashStringKey reinterprets K (known from reflect.Kind to be a string kind) as
// a string without copying. This keeps named string types on the same hot path
// as plain string without reflection or allocation per hash.
func hashStringKey[K comparable](key K) uint64 {
	return hashString(*(*string)(unsafe.Pointer(&key)))
}

func hashIntKey[K comparable, T hashableInteger](key K) uint64 {
	return Avalanche(uint64(*(*T)(unsafe.Pointer(&key))))
}

func hashFallbackKey[K comparable](key K) uint64 {
	return hashString(fmt.Sprintf("%v", key))
}

// Avalanche runs the final xor-shift/multiply chain that enforces avalanche
// behavior. It finalizes integerkey hashes, ghost tags, and sketch indexes, so
// it is compiled in every build regardless of the string-hash backend.
func Avalanche(h64 uint64) uint64 {
	h64 ^= h64 >> avalancheShift1
	h64 *= prime64_2
	h64 ^= h64 >> avalancheShift2
	h64 *= prime64_3
	h64 ^= h64 >> avalancheShift3
	return h64
}
