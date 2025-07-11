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

// represents a single node in the path tree.
// Each node can store multiple cache keys and child nodes for deeper path segments.
type patternNode struct {
	children map[string]*patternNode // Child nodes indexed by path segment
	keys     map[string]bool         // Cache keys stored at this path level
}

// maintains a tree structure that maps URL paths to cache keys,
type patternIndex struct {
	mu   sync.RWMutex // Reader-writer mutex for concurrent access
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

// normalizePath converts a URL path into a slice of path segments for tree traversal.
func normalizePath(path string) []string {
	// Convert empty path to root
	if path == "" {
		path = rootPath
	}

	// Remove leading and trailing slashes
	trimmed := strings.Trim(path, pathSeparator)
	if trimmed == "" {
		return []string{} // Root path results in empty segments
	}

	// Split path into individual segments
	return strings.Split(trimmed, pathSeparator)
}

// associates a cache key with a specific path in the tree.
// creates intermediate nodes as needed when traversing the path.
func (pi *patternIndex) addKey(path, key string) {
	pi.mu.Lock()
	defer pi.mu.Unlock()

	// convert path to segments for tree traversal
	segments := normalizePath(path)
	node := pi.root

	// traverse or create path in the tree
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

// returns all cache keys that match the given pattern.
// supports exact path matching and wildcard patterns ending with '*'.
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
		return pi.collectAllKeys(node) // collect from this node and all children
	}
	return pi.collectDirectKeys(node) // collect only from this node
}

// traverses the tree to find the node at the specified path segments.
func (pi *patternIndex) findNode(segments []string) *patternNode {
	node := pi.root
	for _, segment := range segments {
		if node.children[segment] == nil {
			return nil // Path doesn't exist
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

// collects all keys from the given node and all its descendants.
// used for wildcard pattern matching to gather keys from entire subtrees.
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
