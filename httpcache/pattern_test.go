package httpcache

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"testing"
)

// id returns a distinct response identity for trie tests that don't care about
// the value itself, only that keys are present.
func id() *Response { return &Response{} }

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
		{name: "empty path", path: "", expected: nil},
		{name: "root path", path: "/", expected: nil},
		{name: "single segment", path: "/api", expected: []string{"api"}},
		{name: "multiple segments", path: "/api/v1/users", expected: []string{"api", "v1", "users"}},
		{name: "trailing slash", path: "/api/v1/users/", expected: []string{"api", "v1", "users"}},
		{name: "no leading slash", path: "api/v1/users", expected: []string{"api", "v1", "users"}},
		{name: "multiple slashes", path: "//api//v1//users//", expected: []string{"api", "v1", "users"}},
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

	pi.addKey("/api/v1", "key1", id())
	pi.addKey("/api/v1", "key2", id())
	pi.addKey("/api/v2", "key3", id())
	pi.addKey("/", "rootkey", id())

	keys := pi.getMatchingKeys("/api/v1")
	sort.Strings(keys)
	if expected := []string{"key1", "key2"}; !reflect.DeepEqual(keys, expected) {
		t.Errorf("getMatchingKeys(/api/v1) = %v, want %v", keys, expected)
	}

	if rootKeys := pi.getMatchingKeys("/"); len(rootKeys) != 1 || rootKeys[0] != "rootkey" {
		t.Errorf("root keys = %v, want [rootkey]", rootKeys)
	}
}

// Re-indexing a key at a new path with a new identity replaces the old mapping.
func TestPatternIndex_AddKeyReindexesSamePath(t *testing.T) {
	pi := newPatternIndex()

	first := id()
	second := id()
	pi.addKey("/p", "key1", first)
	pi.addKey("/p", "key1", second)

	keys := pi.getMatchingKeys("/p")
	if len(keys) != 1 || keys[0] != "key1" {
		t.Fatalf("keys = %v, want [key1]", keys)
	}
	// The stale identity must not remove the live entry.
	pi.removeKeyByIdentity("/p", "key1", first)
	if keys := pi.getMatchingKeys("/p"); len(keys) != 1 {
		t.Fatalf("after stale removal keys = %v, want [key1]", keys)
	}
	// The current identity removes it.
	pi.removeKeyByIdentity("/p", "key1", second)
	if keys := pi.getMatchingKeys("/p"); len(keys) != 0 {
		t.Fatalf("after live removal keys = %v, want none", keys)
	}
}

func TestPatternIndex_RemoveByIdentity(t *testing.T) {
	pi := newPatternIndex()

	k1, k2 := id(), id()
	pi.addKey("/api/v1", "key1", k1)
	pi.addKey("/api/v1", "key2", k2)

	// Mismatched identity is a no-op.
	pi.removeKeyByIdentity("/api/v1", "key1", id())
	if keys := pi.getMatchingKeys("/api/v1"); len(keys) != 2 {
		t.Fatalf("mismatched removal changed keys: %v", keys)
	}

	// Matching identity removes the key.
	pi.removeKeyByIdentity("/api/v1", "key1", k1)
	if keys := pi.getMatchingKeys("/api/v1"); len(keys) != 1 || keys[0] != "key2" {
		t.Fatalf("keys = %v, want [key2]", keys)
	}

	// Removing absent keys / paths must not panic.
	pi.removeKeyByIdentity("/api/v1", "missing", id())
	pi.removeKeyByIdentity("/nonexistent", "key2", k2)
}

// removeKeyByIdentity must prune nodes that become empty so the trie does not
// retain dead branches.
func TestPatternIndex_RemoveByIdentityPrunesEmptyBranches(t *testing.T) {
	pi := newPatternIndex()

	users, posts := id(), id()
	pi.addKey("/api/v1/users", "users-key", users)
	pi.addKey("/api/v1/posts", "posts-key", posts)

	pi.removeKeyByIdentity("/api/v1/users", "users-key", users)
	if keys := pi.getMatchingKeys("/api/v1/users"); len(keys) != 0 {
		t.Fatalf("users keys = %v, want none", keys)
	}
	if keys := pi.getMatchingKeys("/api/v1/posts"); len(keys) != 1 {
		t.Fatalf("posts keys = %v, want [posts-key]", keys)
	}

	pi.removeKeyByIdentity("/api/v1/posts", "posts-key", posts)
	if len(pi.root.children) != 0 {
		t.Fatalf("root children = %v, want fully pruned trie", pi.root.children)
	}
}

func TestPatternIndex_GetMatchingKeys(t *testing.T) {
	pi := newPatternIndex()

	pi.addKey("/api/v1/users", "users-key1", id())
	pi.addKey("/api/v1/users", "users-key2", id())
	pi.addKey("/api/v1/posts", "posts-key1", id())
	pi.addKey("/api/v2/users", "v2-users-key1", id())
	pi.addKey("/static/css", "css-key1", id())
	pi.addKey("/", "root-key", id())

	tests := []struct {
		name     string
		pattern  string
		expected []string
	}{
		{name: "exact match", pattern: "/api/v1/users", expected: []string{"users-key1", "users-key2"}},
		{name: "wildcard match", pattern: "/api/v1/*", expected: []string{"users-key1", "users-key2", "posts-key1"}},
		{name: "broader wildcard", pattern: "/api/*", expected: []string{"users-key1", "users-key2", "posts-key1", "v2-users-key1"}},
		{name: "root wildcard", pattern: "/*", expected: []string{"users-key1", "users-key2", "posts-key1", "v2-users-key1", "css-key1", "root-key"}},
		{name: "no match", pattern: "/nonexistent", expected: nil},
		{name: "root exact", pattern: "/", expected: []string{"root-key"}},
		{name: "empty pattern", pattern: "", expected: []string{"root-key"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := pi.getMatchingKeys(tt.pattern)
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

	pi.addKey("/api/v1", "key1", id())
	pi.addKey("/api/v2", "key2", id())
	if keys := pi.getMatchingKeys("/*"); len(keys) == 0 {
		t.Fatal("expected keys before clear")
	}

	pi.clear()

	if keys := pi.getMatchingKeys("/*"); len(keys) != 0 {
		t.Errorf("expected no keys after clear, got %v", keys)
	}

	// Index is still usable after clear.
	pi.addKey("/test", "test-key", id())
	if keys := pi.getMatchingKeys("/test"); len(keys) != 1 || keys[0] != "test-key" {
		t.Errorf("keys after clear+add = %v, want [test-key]", keys)
	}
}

func TestPatternIndex_ConcurrentAccess(t *testing.T) {
	pi := newPatternIndex()
	shared := id()
	done := make(chan bool)

	go func() {
		for i := 0; i < 100; i++ {
			pi.addKey("/api/test", fmt.Sprintf("key%d", i), shared)
		}
		done <- true
	}()
	go func() {
		for i := 0; i < 100; i++ {
			pi.getMatchingKeys("/api/*")
		}
		done <- true
	}()

	<-done
	<-done

	if keys := pi.getMatchingKeys("/api/test"); len(keys) != 100 {
		t.Errorf("expected 100 keys, got %d", len(keys))
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
	if len(node.children) != 0 || len(node.keys) != 0 {
		t.Errorf("expected empty node, got %d children / %d keys", len(node.children), len(node.keys))
	}
}

func BenchmarkPatternIndex_AddKey(b *testing.B) {
	b.Run("unique-precomputed", func(b *testing.B) {
		keys := benchmarkPatternKeys(b.N, "key")
		pi := newPatternIndex()
		shared := id()

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			pi.addKey("/api/v1/users", keys[i], shared)
		}
	})

	b.Run("overwrite-same-key", func(b *testing.B) {
		pi := newPatternIndex()
		shared := id()

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			pi.addKey("/api/v1/users", "key", shared)
		}
	})
}

func benchmarkPatternKeys(n int, prefix string) []string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = prefix + strconv.Itoa(i)
	}
	return keys
}

func BenchmarkPatternIndex_GetMatchingKeys(b *testing.B) {
	pi := newPatternIndex()
	shared := id()
	for i := 0; i < 1000; i++ {
		pi.addKey("/api/v1/users", fmt.Sprintf("key%d", i), shared)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pi.getMatchingKeys("/api/v1/users")
	}
}

func BenchmarkPatternIndex_WildcardMatch(b *testing.B) {
	pi := newPatternIndex()
	shared := id()
	for i := 0; i < 100; i++ {
		pi.addKey("/api/v1/users", fmt.Sprintf("users-key%d", i), shared)
		pi.addKey("/api/v1/posts", fmt.Sprintf("posts-key%d", i), shared)
		pi.addKey("/api/v2/users", fmt.Sprintf("v2-users-key%d", i), shared)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pi.getMatchingKeys("/api/*")
	}
}
