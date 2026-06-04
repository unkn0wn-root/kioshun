//go:build !kioshun_purego

package keyhash

import "unsafe"

// hashString hashes a string with the Go runtime's memhash.
// This is the default backend. Build with -tags kioshun_purego to
// use the pure-Go xxHash64/FNV implementation in stringhash_purego.go instead.
// The seed is fixed at 0, but memhash still mixes in the runtime's per-process
// hash key. Hashes are therefore stable for the lifetime of the process (all the
// cache needs - they are never persisted) yet differ between runs.
// Hashing only reads the string's bytes and never retains the pointer so
// aliasing the backing array here is safe and avoids allocation.
func hashString(s string) uint64 {
	if len(s) == 0 {
		// unsafe.StringData is unspecified for the empty hashString
		// short-circuit instead of handing a possibly-nil pointer.
		return 0
	}
	return uint64(runtimeMemhash(unsafe.Pointer(unsafe.StringData(s)), 0, uintptr(len(s))))
}

// runtimeMemhash is runtime.memhash(p, seed, len).
// The pointer is readonly and not retained so the call cannot leak it.
//
//go:noescape
//go:linkname runtimeMemhash runtime.memhash
func runtimeMemhash(p unsafe.Pointer, seed, length uintptr) uintptr
