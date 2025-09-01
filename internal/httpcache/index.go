package httpcache

import (
	"strings"
	"sync"
)

const (
	rootPath      = "/" // Default path when none specified
	pathSeparator = "/" // URL path separator
	wildcardChar  = "*" // Wildcard character for pattern matching
)

// PatternNode represents a single node in the path tree.
type PatternNode struct {
	children map[string]*PatternNode
	keys     map[string]bool
}

// PatternIndex maintains a tree structure that maps URL paths to cache keys.
type PatternIndex struct {
	mu   sync.RWMutex
	root *PatternNode
}

func NewPatternIndex() *PatternIndex {
	return &PatternIndex{root: newPatternNode()}
}

func newPatternNode() *PatternNode {
	return &PatternNode{
		children: make(map[string]*PatternNode),
		keys:     make(map[string]bool),
	}
}

// DefaultPathExtractor returns an empty string; override to map keys to paths.
func DefaultPathExtractor(key string) string { return "" }

// NormalizePath converts a URL path into a normalized slice of path segments
// Empty path handling:
// - Empty string converts to root path ("/")
// - Ensures all paths have a canonical representation
//
// Path cleaning process:
// 1. Trim leading and trailing slashes to remove "/path/" -> "path"
// 2. After trimming, empty result indicates root path (returns empty slice)
// 3. Split remaining path by separator into individual segments
// 4. Filter out empty segments caused by double slashes ("//") or malformed paths
//
// Examples:
// - "" -> []
// - "/" -> []
// - "/api/v1/" -> ["api", "v1"]
// - "//api//v1//" -> ["api", "v1"]
// - "api/v1" -> ["api", "v1"]
//
// This normalization ensures that equivalent paths (with different slash patterns)
// map to the same tree location, preventing duplicate entries
func normalizePath(path string) []string {
	if path == "" {
		path = rootPath
	}

	trimmed := strings.Trim(path, pathSeparator)
	if trimmed == "" {
		return []string{}
	}

	segments := strings.Split(trimmed, pathSeparator)
	result := make([]string, 0, len(segments))
	for _, seg := range segments {
		if seg != "" {
			result = append(result, seg)
		}
	}
	return result
}

// AddKey associates a cache key with a specific path in the trie.
func (pi *PatternIndex) AddKey(path, key string) {
	pi.mu.Lock()
	defer pi.mu.Unlock()

	node := pi.root
	segments := normalizePath(path)
	for _, s := range segments {
		if node.children[s] == nil {
			node.children[s] = newPatternNode()
		}
		node = node.children[s]
	}
	node.keys[key] = true
}

// RemoveKey removes a cache key from the specified path.
func (pi *PatternIndex) RemoveKey(path, key string) {
	pi.mu.Lock()
	defer pi.mu.Unlock()

	segments := normalizePath(path)
	node := pi.findNode(segments)
	if node != nil {
		delete(node.keys, key)
	}
}

// GetMatchingKeys returns all cache keys that match the given pattern
//
// Exact path matching:
// - Pattern without '*' suffix matches only keys stored at that exact path
// - Uses findNode() to locate the specific tree node
// - Collects only keys stored directly at the target node
// - Example: "/api/users" matches keys at exactly "/api/users"
//
// Wildcard pattern matching:
// - Pattern ending with '*' enables prefix-based subtree matching
// - Strips the '*' suffix and finds the base path node
// - Recursively collects keys from the base node and all descendant nodes
// - Example: "/api/*" matches keys at "/api", "/api/users", "/api/users/123", etc.
func (pi *PatternIndex) GetMatchingKeys(pattern string) []string {
	pi.mu.RLock()
	defer pi.mu.RUnlock()

	if pattern == "" {
		pattern = rootPath
	}

	isWildcard := strings.HasSuffix(pattern, wildcardChar)
	if isWildcard {
		pattern = strings.TrimSuffix(pattern, wildcardChar)
	}

	segments := normalizePath(pattern)
	node := pi.findNode(segments)
	if node == nil {
		return nil
	}
	if isWildcard {
		return pi.collectAllKeys(node)
	}
	return pi.collectDirectKeys(node)
}

func (pi *PatternIndex) findNode(segments []string) *PatternNode {
	node := pi.root
	for _, s := range segments {
		next := node.children[s]
		if next == nil {
			return nil
		}
		node = next
	}
	return node
}

func (pi *PatternIndex) collectDirectKeys(node *PatternNode) []string {
	if len(node.keys) == 0 {
		return nil
	}

	keys := make([]string, 0, len(node.keys))
	for k := range node.keys {
		keys = append(keys, k)
	}
	return keys
}

func (pi *PatternIndex) collectAllKeys(node *PatternNode) []string {
	var keys []string
	for k := range node.keys {
		keys = append(keys, k)
	}
	for _, c := range node.children {
		keys = append(keys, pi.collectAllKeys(c)...)
	}
	return keys
}

// Clear resets the index to empty state.
func (pi *PatternIndex) Clear() {
	pi.mu.Lock()
	pi.root = newPatternNode()
	pi.mu.Unlock()
}
