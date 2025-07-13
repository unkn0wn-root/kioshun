package cache

import (
	"strings"
	"sync"
)

const (
	rootPath      = "/" // Default path when none specified
	pathSeparator = "/" // URL path separator
	wildcardChar  = "*" // Wildcard character for pattern matching
)

// Represents a single node in the path tree.
// Each node can store multiple cache keys and child nodes for deeper path segments.
type patternNode struct {
	children map[string]*patternNode // Child nodes indexed by path segment
	keys     map[string]bool         // Cache keys stored at this path level
}

// maintains a tree structure that maps URL paths to cache keys,
type patternIndex struct {
	mu   sync.RWMutex
	root *patternNode // Root node of the path tree
}

func newPatternIndex() *patternIndex {
	return &patternIndex{
		root: newPatternNode(),
	}
}

func newPatternNode() *patternNode {
	return &patternNode{
		children: make(map[string]*patternNode),
		keys:     make(map[string]bool),
	}
}

// normalizePath converts a URL path into a normalized slice of path segments
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
		return []string{} // Root path results in empty segments
	}

	segments := strings.Split(trimmed, pathSeparator)
	result := make([]string, 0, len(segments))
	for _, segment := range segments {
		if segment != "" {
			result = append(result, segment)
		}
	}
	return result
}

// addKey associates a cache key with a specific path in the trie, creating intermediate nodes as needed
//
// Path processing:
// 1. Normalizes the input path into segments using normalizePath()
// 2. Starts traversal from the root node of the trie
// 3. For each path segment, checks if child node exists
//
// Lazy node creation:
// - If child node doesn't exist for a segment, creates new patternNode on-demand
// - This ensures the tree only grows as paths are actually added
func (pi *patternIndex) addKey(path, key string) {
	pi.mu.Lock()
	defer pi.mu.Unlock()

	node := pi.root

	segments := normalizePath(path)
	for _, segment := range segments {
		if node.children[segment] == nil {
			node.children[segment] = newPatternNode()
		}
		node = node.children[segment]
	}

	node.keys[key] = true
}

// removes a cache key from the specified path.
// does nothing if the path or key doesn't exist.
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
		return pi.collectAllKeys(node) // all children
	}
	return pi.collectDirectKeys(node) // this node
}

// findNode traverses the trie to locate the node corresponding to the given path segments
//
// 1. Starts at root node and iterates through each path segment
// 2. For each segment, checks if corresponding child node exists
// 3. If child exists, moves to that node and continues
// 4. If child doesn't exist, immediately returns nil (path not found)
//
// - Stops traversal as soon as a missing path segment is encountered
// - O(d) time complexity where d is depth to first missing segment
func (pi *patternIndex) findNode(segments []string) *patternNode {
	node := pi.root
	for _, segment := range segments {
		if node.children[segment] == nil {
			return nil
		}
		node = node.children[segment]
	}
	return node
}

// returns all keys stored directly at the given node.
func (pi *patternIndex) collectDirectKeys(node *patternNode) []string {
	if len(node.keys) == 0 {
		return nil
	}

	keys := make([]string, 0, len(node.keys))
	for key := range node.keys {
		keys = append(keys, key)
	}
	return keys
}

// collectAllKeys recursively gathers all cache keys from a node and its entire subtree
//
// 1. Collects all keys stored directly at the current node
// 2. Recursively visits each child node to collect their keys
// 3. Combines all collected keys into a single flat slice
//
// - Depth-first approach ensures complete subtree coverage
// - No specific ordering of keys (map iteration order is not guaranteed)
// - O(n) where n is total number of keys in subtree
func (pi *patternIndex) collectAllKeys(node *patternNode) []string {
	var keys []string

	for key := range node.keys {
		keys = append(keys, key)
	}

	for _, child := range node.children {
		keys = append(keys, pi.collectAllKeys(child)...)
	}

	return keys
}

// resets the pattern index by replacing the root with a new empty node.
// this removes all stored paths and keys from the index.
func (pi *patternIndex) clear() {
	pi.mu.Lock()
	defer pi.mu.Unlock()

	pi.root = newPatternNode()
}
