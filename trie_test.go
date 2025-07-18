package cache

import (
	"reflect"
	"sort"
	"testing"
)

func TestNewPatternIndex(t *testing.T) {
	pi := newPatternIndex()

	if pi == nil {
		t.Fatal("newPatternIndex() returned nil")
	}

	if pi.root == nil {
		t.Fatal("root node is nil")
	}

	if pi.root.children == nil {
		t.Fatal("root children map is nil")
	}

	if pi.root.keys == nil {
		t.Fatal("root keys map is nil")
	}
}

func TestNormalizePath(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		expected []string
	}{
		{
			name:     "empty path",
			path:     "",
			expected: []string{},
		},
		{
			name:     "root path",
			path:     "/",
			expected: []string{},
		},
		{
			name:     "single segment",
			path:     "/api",
			expected: []string{"api"},
		},
		{
			name:     "multiple segments",
			path:     "/api/v1/users",
			expected: []string{"api", "v1", "users"},
		},
		{
			name:     "trailing slash",
			path:     "/api/v1/users/",
			expected: []string{"api", "v1", "users"},
		},
		{
			name:     "no leading slash",
			path:     "api/v1/users",
			expected: []string{"api", "v1", "users"},
		},
		{
			name:     "multiple slashes",
			path:     "//api//v1//users//",
			expected: []string{"api", "v1", "users"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := normalizePath(tt.path)
			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("normalizePath(%q) = %v, expected %v", tt.path, result, tt.expected)
			}
		})
	}
}

func TestPatternIndex_AddKey(t *testing.T) {
	pi := newPatternIndex()

	// Test adding keys to different paths
	pi.addKey("/api/v1", "key1")
	pi.addKey("/api/v1", "key2")
	pi.addKey("/api/v2", "key3")
	pi.addKey("/", "rootkey")

	// Verify keys were added correctly
	keys := pi.getMatchingKeys("/api/v1")
	sort.Strings(keys)
	expected := []string{"key1", "key2"}
	sort.Strings(expected)

	if !reflect.DeepEqual(keys, expected) {
		t.Errorf("Expected keys %v, got %v", expected, keys)
	}

	// Test root path
	rootKeys := pi.getMatchingKeys("/")
	if len(rootKeys) != 1 || rootKeys[0] != "rootkey" {
		t.Errorf("Expected root key [rootkey], got %v", rootKeys)
	}
}

func TestPatternIndex_RemoveKey(t *testing.T) {
	pi := newPatternIndex()

	// Add some keys
	pi.addKey("/api/v1", "key1")
	pi.addKey("/api/v1", "key2")
	pi.addKey("/api/v2", "key3")

	// Remove one key
	pi.removeKey("/api/v1", "key1")

	// Verify key was removed
	keys := pi.getMatchingKeys("/api/v1")
	if len(keys) != 1 || keys[0] != "key2" {
		t.Errorf("Expected [key2], got %v", keys)
	}

	// Remove non-existent key (should not panic)
	pi.removeKey("/api/v1", "nonexistent")

	// Remove from non-existent path (should not panic)
	pi.removeKey("/nonexistent", "key1")
}

func TestPatternIndex_GetMatchingKeys(t *testing.T) {
	pi := newPatternIndex()

	// Add test data
	pi.addKey("/api/v1/users", "users-key1")
	pi.addKey("/api/v1/users", "users-key2")
	pi.addKey("/api/v1/posts", "posts-key1")
	pi.addKey("/api/v2/users", "v2-users-key1")
	pi.addKey("/static/css", "css-key1")
	pi.addKey("/", "root-key")

	tests := []struct {
		name     string
		pattern  string
		expected []string
	}{
		{
			name:     "exact match",
			pattern:  "/api/v1/users",
			expected: []string{"users-key1", "users-key2"},
		},
		{
			name:     "wildcard match",
			pattern:  "/api/v1/*",
			expected: []string{"users-key1", "users-key2", "posts-key1"},
		},
		{
			name:     "broader wildcard",
			pattern:  "/api/*",
			expected: []string{"users-key1", "users-key2", "posts-key1", "v2-users-key1"},
		},
		{
			name:     "root wildcard",
			pattern:  "/*",
			expected: []string{"users-key1", "users-key2", "posts-key1", "v2-users-key1", "css-key1", "root-key"},
		},
		{
			name:     "no match",
			pattern:  "/nonexistent",
			expected: nil,
		},
		{
			name:     "root exact",
			pattern:  "/",
			expected: []string{"root-key"},
		},
		{
			name:     "empty pattern",
			pattern:  "",
			expected: []string{"root-key"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := pi.getMatchingKeys(tt.pattern)

			// Sort both slices for comparison
			sort.Strings(result)
			sort.Strings(tt.expected)

			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("getMatchingKeys(%q) = %v, expected %v", tt.pattern, result, tt.expected)
			}
		})
	}
}

func TestPatternIndex_Clear(t *testing.T) {
	pi := newPatternIndex()

	// Add some data
	pi.addKey("/api/v1", "key1")
	pi.addKey("/api/v2", "key2")

	// Verify data exists
	keys := pi.getMatchingKeys("/*")
	if len(keys) == 0 {
		t.Fatal("Expected keys before clear")
	}

	// Clear the index
	pi.clear()

	// Verify everything is cleared
	keys = pi.getMatchingKeys("/*")
	if len(keys) != 0 {
		t.Errorf("Expected no keys after clear, got %v", keys)
	}

	// Verify we can still add keys after clear
	pi.addKey("/test", "test-key")
	keys = pi.getMatchingKeys("/test")
	if len(keys) != 1 || keys[0] != "test-key" {
		t.Errorf("Expected [test-key] after clear and add, got %v", keys)
	}
}

func TestPatternIndex_ConcurrentAccess(t *testing.T) {
	pi := newPatternIndex()

	// Test concurrent reads and writes
	done := make(chan bool)

	// Writer goroutine
	go func() {
		for i := 0; i < 100; i++ {
			pi.addKey("/api/test", "key"+string(rune(i)))
		}
		done <- true
	}()

	// Reader goroutine
	go func() {
		for i := 0; i < 100; i++ {
			pi.getMatchingKeys("/api/*")
		}
		done <- true
	}()

	// Wait for both goroutines
	<-done
	<-done

	// Verify final state
	keys := pi.getMatchingKeys("/api/test")
	if len(keys) != 100 {
		t.Errorf("Expected 100 keys, got %d", len(keys))
	}
}

func TestPatternNode_Creation(t *testing.T) {
	node := newPatternNode()

	if node == nil {
		t.Fatal("newPatternNode() returned nil")
	}

	if node.children == nil {
		t.Fatal("children map is nil")
	}

	if node.keys == nil {
		t.Fatal("keys map is nil")
	}

	if len(node.children) != 0 {
		t.Errorf("Expected empty children map, got %d items", len(node.children))
	}

	if len(node.keys) != 0 {
		t.Errorf("Expected empty keys map, got %d items", len(node.keys))
	}
}

func TestPatternIndex_EdgeCases(t *testing.T) {
	pi := newPatternIndex()

	// Test with special characters in paths
	pi.addKey("/api/users@domain.com", "email-key")
	pi.addKey("/api/users+special", "special-key")
	pi.addKey("/api/users%20space", "space-key")

	// Test retrieval
	keys := pi.getMatchingKeys("/api/users@domain.com")
	if len(keys) != 1 || keys[0] != "email-key" {
		t.Errorf("Expected [email-key], got %v", keys)
	}

	// Test wildcard with special characters
	allKeys := pi.getMatchingKeys("/api/*")
	if len(allKeys) != 3 {
		t.Errorf("Expected 3 keys, got %d", len(allKeys))
	}
}

func BenchmarkPatternIndex_AddKey(b *testing.B) {
	pi := newPatternIndex()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pi.addKey("/api/v1/users", "key"+string(rune(i)))
	}
}

func BenchmarkPatternIndex_GetMatchingKeys(b *testing.B) {
	pi := newPatternIndex()

	// Setup test data
	for i := 0; i < 1000; i++ {
		pi.addKey("/api/v1/users", "key"+string(rune(i)))
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pi.getMatchingKeys("/api/v1/users")
	}
}

func BenchmarkPatternIndex_WildcardMatch(b *testing.B) {
	pi := newPatternIndex()

	// Setup test data
	for i := 0; i < 100; i++ {
		pi.addKey("/api/v1/users", "users-key"+string(rune(i)))
		pi.addKey("/api/v1/posts", "posts-key"+string(rune(i)))
		pi.addKey("/api/v2/users", "v2-users-key"+string(rune(i)))
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pi.getMatchingKeys("/api/*")
	}
}
