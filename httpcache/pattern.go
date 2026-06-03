package httpcache

import (
	"strings"
	"sync"
)

const (
	rootPath      = "/"
	pathSeparator = "/"
	wildcardChar  = "*"
)

type patternNode struct {
	children map[string]*patternNode
	keys     map[string]bool
}

type patternIndex struct {
	mu   sync.RWMutex
	root *patternNode
}

func newPatternIndex() *patternIndex {
	return &patternIndex{root: newPatternNode()}
}

func newPatternNode() *patternNode {
	return &patternNode{
		children: make(map[string]*patternNode),
		keys:     make(map[string]bool),
	}
}

func defaultPathExtractor(_ string) string { return "" }

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

func (pi *patternIndex) addKey(path, key string) {
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

func (pi *patternIndex) removeKey(path, key string) {
	pi.mu.Lock()
	defer pi.mu.Unlock()

	segments := normalizePath(path)
	node := pi.findNode(segments)
	if node != nil {
		delete(node.keys, key)
	}
}

// getMatchingKeys returns all cache keys that match the given pattern
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
func (pi *patternIndex) getMatchingKeys(pattern string) []string {
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

func (pi *patternIndex) findNode(segments []string) *patternNode {
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

func (pi *patternIndex) collectDirectKeys(node *patternNode) []string {
	if len(node.keys) == 0 {
		return nil
	}

	keys := make([]string, 0, len(node.keys))
	for k := range node.keys {
		keys = append(keys, k)
	}
	return keys
}

func (pi *patternIndex) collectAllKeys(node *patternNode) []string {
	keys := make([]string, 0, len(node.keys))
	for k := range node.keys {
		keys = append(keys, k)
	}
	for _, c := range node.children {
		keys = append(keys, pi.collectAllKeys(c)...)
	}
	return keys
}

func (pi *patternIndex) clear() {
	pi.mu.Lock()
	pi.root = newPatternNode()
	pi.mu.Unlock()
}
