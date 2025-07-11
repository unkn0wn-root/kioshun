package cache

import (
	"strconv"
	"testing"
)

func TestFnvHash64(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "empty string",
			input: "",
		},
		{
			name:  "single character",
			input: "a",
		},
		{
			name:  "short string",
			input: "test",
		},
		{
			name:  "longer string",
			input: "Hello, World!",
		},
		{
			name:  "numeric string",
			input: "12345",
		},
		{
			name:  "special characters",
			input: "!@#$%^&*()",
		},
		{
			name:  "unicode characters",
			input: "🚀🌟💫",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := fnvHash64(tt.input)
			if result == 0 && tt.input != "" {
				t.Errorf("fnvHash64(%q) returned 0 unexpectedly", tt.input)
			}
		})
	}
}

func TestFnvHash64_Consistency(t *testing.T) {
	// same input always produces the same hash
	testStrings := []string{
		"",
		"a",
		"test",
		"Hello, World!",
		"This is a longer test string with various characters!@#$%^&*()",
		"unicode: 🚀🌟💫✨🎯",
	}

	for _, str := range testStrings {
		t.Run("consistency_"+str, func(t *testing.T) {
			hash1 := fnvHash64(str)
			hash2 := fnvHash64(str)
			hash3 := fnvHash64(str)

			if hash1 != hash2 || hash2 != hash3 {
				t.Errorf("fnvHash64(%q) not consistent: %d, %d, %d", str, hash1, hash2, hash3)
			}
		})
	}
}

func TestFnvHash64_Distribution(t *testing.T) {
	// similar strings produce different hashes
	testPairs := []struct {
		str1, str2 string
	}{
		{"abc", "abd"},
		{"test", "Test"},
		{"hello", "Hello"},
		{"123", "124"},
		{"", " "},
		{"a", "aa"},
		{"hello world", "hello world "},
	}

	for _, pair := range testPairs {
		t.Run("distribution_"+pair.str1+"_vs_"+pair.str2, func(t *testing.T) {
			hash1 := fnvHash64(pair.str1)
			hash2 := fnvHash64(pair.str2)

			if hash1 == hash2 {
				t.Errorf("fnvHash64(%q) == fnvHash64(%q) = %d (collision)", pair.str1, pair.str2, hash1)
			}
		})
	}
}

func TestFnvHash64_CaseSensitive(t *testing.T) {
	// hash is case-sensitive
	testCases := []struct {
		lower, upper string
	}{
		{"hello", "HELLO"},
		{"world", "WORLD"},
		{"test", "TEST"},
		{"abc", "ABC"},
	}

	for _, tc := range testCases {
		t.Run("case_sensitive_"+tc.lower, func(t *testing.T) {
			lowerHash := fnvHash64(tc.lower)
			upperHash := fnvHash64(tc.upper)

			if lowerHash == upperHash {
				t.Errorf("fnvHash64 is not case-sensitive: %q and %q have same hash %d", tc.lower, tc.upper, lowerHash)
			}
		})
	}
}

func TestFnvHash64_LongStrings(t *testing.T) {
	longStr := ""
	for i := 0; i < 10000; i++ {
		longStr += "a"
	}

	hash := fnvHash64(longStr)

	// Should not panic and should return a valid hash
	if hash == 0 {
		t.Error("fnvHash64 returned 0 for long string")
	}

	// Test consistency with long strings
	hash2 := fnvHash64(longStr)
	if hash != hash2 {
		t.Error("fnvHash64 not consistent with long strings")
	}
}

func TestFnvHash64_BinaryData(t *testing.T) {
	// Test with binary data (null bytes, control characters)
	binaryData := []string{
		"\x00\x01\x02\x03",
		"\xff\xfe\xfd\xfc",
		"\x00hello\x00world\x00",
		string([]byte{0, 255, 128, 64, 32, 16, 8, 4, 2, 1}),
	}

	for i, data := range binaryData {
		t.Run("binary_data_"+strconv.Itoa(i), func(t *testing.T) {
			hash := fnvHash64(data)

			// Should not panic and should return a valid hash
			if hash == 0 {
				t.Error("fnvHash64 returned 0 for binary data")
			}

			// Test consistency
			hash2 := fnvHash64(data)
			if hash != hash2 {
				t.Error("fnvHash64 not consistent with binary data")
			}
		})
	}
}

func TestFnvHash64_EdgeCases(t *testing.T) {
	// Test edge cases
	edgeCases := []string{
		"",           // empty string
		" ",          // single space
		"\n",         // newline
		"\t",         // tab
		"\r\n",       // windows line ending
		"a",          // single character
		"aa",         // repeated character
		"aaa",        // multiple repeated characters
		"aaaa",       // even more repeated characters
		"abcd",       // simple sequence
		"dcba",       // reverse sequence
		"1234567890", // numbers
		"!@#$%^&*()", // special characters
	}

	hashes := make(map[uint64]string)

	for _, str := range edgeCases {
		t.Run("edge_case_"+str, func(t *testing.T) {
			hash := fnvHash64(str)

			// Check for collisions among edge cases
			if existing, exists := hashes[hash]; exists {
				t.Errorf("Hash collision: %q and %q both hash to %d", str, existing, hash)
			}
			hashes[hash] = str

			// Verify consistency
			hash2 := fnvHash64(str)
			if hash != hash2 {
				t.Errorf("fnvHash64(%q) not consistent: %d != %d", str, hash, hash2)
			}
		})
	}
}

func TestFnvHash64_Performance(t *testing.T) {
	// Test that the function doesn't have obvious performance issues
	testStr := "This is a test string for performance testing"

	// Run multiple iterations to check for performance consistency
	for i := 0; i < 1000; i++ {
		hash := fnvHash64(testStr)
		if hash == 0 {
			t.Error("Unexpected zero hash")
		}
	}
}

// Benchmark tests
func BenchmarkFnvHash64_Short(b *testing.B) {
	s := "test"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fnvHash64(s)
	}
}

func BenchmarkFnvHash64_Medium(b *testing.B) {
	s := "This is a medium length string for benchmarking"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fnvHash64(s)
	}
}

func BenchmarkFnvHash64_Long(b *testing.B) {
	s := ""
	for i := 0; i < 1000; i++ {
		s += "a"
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fnvHash64(s)
	}
}

func BenchmarkFnvHash64_VeryLong(b *testing.B) {
	s := ""
	for i := 0; i < 10000; i++ {
		s += "test string "
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fnvHash64(s)
	}
}

func BenchmarkFnvHash64_Empty(b *testing.B) {
	s := ""
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fnvHash64(s)
	}
}

func BenchmarkFnvHash64_SingleChar(b *testing.B) {
	s := "a"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fnvHash64(s)
	}
}

func BenchmarkFnvHash64_URL(b *testing.B) {
	s := "https://example.com/api/v1/users/123?param=value&another=test"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fnvHash64(s)
	}
}

func BenchmarkFnvHash64_UUID(b *testing.B) {
	s := "550e8400-e29b-41d4-a716-446655440000"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fnvHash64(s)
	}
}
