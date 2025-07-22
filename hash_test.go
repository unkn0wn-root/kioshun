package cache

import (
	"fmt"
	"testing"
)

var testStrings = []string{
	"a",                                  // 1 byte
	"test",                               // 4 bytes
	"testkey",                            // 7 bytes
	"testkey1",                           // 8 bytes
	"testkey12",                          // 9 bytes
	"user:profile:12345",                 // 18 bytes
	"cache:session:user:1234567890:data", // 34 bytes
	"this:is:a:very:long:cache:key:that:represents:typical:usage:in:high:performance:systems", // 89 bytes
}

// Test that hash function produces consistent results
func TestHashConsistency(t *testing.T) {
	h := newHasher[string]()

	for _, str := range testStrings {
		hash1 := h.hash(str)
		hash2 := h.hash(str)

		if hash1 != hash2 {
			t.Errorf("Hash function not consistent for string %q: got %v and %v", str, hash1, hash2)
		}
	}
}

// Test that different strings produce different hashes (basic collision test)
func TestHashDistribution(t *testing.T) {
	h := newHasher[string]()
	hashes := make(map[uint64]string)

	for _, str := range testStrings {
		hash := h.hash(str)
		if existing, exists := hashes[hash]; exists {
			t.Errorf("Hash collision: %q and %q both hash to %v", str, existing, hash)
		}
		hashes[hash] = str
	}
}

// Test integer hashing
func TestIntegerHashing(t *testing.T) {
	h := newHasher[int]()

	testInts := []int{0, 1, 42, 1000, -1, -42}
	hashes := make(map[uint64]int)

	for _, num := range testInts {
		hash := h.hash(num)
		if existing, exists := hashes[hash]; exists {
			t.Errorf("Hash collision: %d and %d both hash to %v", num, existing, hash)
		}
		hashes[hash] = num
	}
}

func TestHybridThreshold(t *testing.T) {
	h := newHasher[string]()

	shortString := "short"                                                       // 5 bytes - should use FNV
	longString := "this_is_a_very_long_string_that_exceeds_the_threshold_length" // >32 bytes - should use xxHash

	shortHash := h.hash(shortString)
	longHash := h.hash(longString)

	if shortHash == 0 || longHash == 0 {
		t.Error("Hash functions should not produce zero hashes for non-empty strings")
	}

	if shortHash == longHash {
		t.Error("Different strings should produce different hashes")
	}
}

func BenchmarkHasherString(t *testing.B) {
	h := newHasher[string]()

	for _, str := range testStrings {
		t.Run(fmt.Sprintf("len_%d", len(str)), func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = h.hash(str)
			}
		})
	}
}

func BenchmarkRealisticWorkload(t *testing.B) {
	h := newHasher[string]()

	workloadKeys := []string{
		"u:1",                           // Very short user ID
		"user:1234",                     // Short user key
		"session:abc123def456",          // Medium session key
		"cache:user:profile:1234567890", // Long structured key
		"api:v1:endpoint:users:get:with:filters:and:pagination:page:1:limit:50", // Very long API key
	}

	t.ResetTimer()
	for i := 0; i < t.N; i++ {
		key := workloadKeys[i%len(workloadKeys)]
		_ = h.hash(key)
	}
}

func BenchmarkHashDistribution(t *testing.B) {
	h := newHasher[string]()

	// Generate keys with common prefixes to test collision resistance
	keys := make([]string, 1000)
	for i := range keys {
		keys[i] = fmt.Sprintf("user:session:id:%d:data", i)
	}

	t.ResetTimer()
	collisions := make(map[uint64]int)
	for i := 0; i < t.N && i < len(keys); i++ {
		hash := h.hash(keys[i])
		collisions[hash]++
	}
}
