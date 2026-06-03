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

// patternIndex is a path segment trie mapping normalized URL paths to the cache
// keys stored under them. It backs Middleware.Invalidate: an exact pattern
// ("/api/users") matches keys stored at that path, and a trailing wildcard
// ("/api/*") matches the base path and every descendant.
//
// The index is kept in sync with the backing cache by the cache's eviction
// listener. addKey records a key when its response is cached; removeKeyByIdentity
// drops it when the cache evicts, expires, or deletes it. Those notifications are
// delivered asynchronously, so a key may be recached (producing a new *Response)
// before the notification for its previous incarnation is processed. Each key is
// therefore stored alongside the *Response it was indexed with as an identity
// token, and removeKeyByIdentity removes only on a token match - a stale
// notification can never drop a live entry.
type patternIndex struct {
	mu   sync.RWMutex
	root *patternNode
}

// patternNode is one path segment. keys holds the cache keys stored exactly at
// this node's path, each mapped to the response identity recorded by addKey.
type patternNode struct {
	children map[string]*patternNode
	keys     map[string]*Response
}

func newPatternIndex() *patternIndex {
	return &patternIndex{root: newPatternNode()}
}

func newPatternNode() *patternNode {
	return &patternNode{
		children: make(map[string]*patternNode),
		keys:     make(map[string]*Response),
	}
}

// normalizePath splits a URL path into its non-empty segments so that equivalent
// paths ("/api/v1", "/api/v1/", "//api//v1//") resolve to the same trie node.
// The root path ("" or "/") yields no segments.
func normalizePath(path string) []string {
	trimmed := strings.Trim(path, pathSeparator)
	if trimmed == "" {
		return nil
	}

	segments := strings.Split(trimmed, pathSeparator)
	// filter empty segments (from internal "//") in place; segments is freshly
	// allocated by Split, and writes never outrun reads.
	out := segments[:0]
	for _, seg := range segments {
		if seg != "" {
			out = append(out, seg)
		}
	}
	return out
}

// addKey records that key, holding response id, is cached at path.
func (pi *patternIndex) addKey(path, key string, id *Response) {
	segments := normalizePath(path)

	pi.mu.Lock()
	defer pi.mu.Unlock()

	node := pi.root
	for _, seg := range segments {
		child := node.children[seg]
		if child == nil {
			child = newPatternNode()
			node.children[seg] = child
		}
		node = child
	}
	node.keys[key] = id
}

// removeKeyByIdentity drops key from path only if it is still indexed with
// identity id, then prunes any trie branch left empty. A mismatched (or missing)
// identity means the key was recached after this removal was queued, so the live
// entry is left in place.
func (pi *patternIndex) removeKeyByIdentity(path, key string, id *Response) {
	segments := normalizePath(path)

	pi.mu.Lock()
	defer pi.mu.Unlock()

	// descend to the target node, remembering the parent of each step for pruning.
	node := pi.root
	parents := make([]*patternNode, len(segments))
	for i, seg := range segments {
		child := node.children[seg]
		if child == nil {
			return
		}
		parents[i] = node
		node = child
	}

	if node.keys[key] != id {
		return
	}
	delete(node.keys, key)

	// prune empty nodes from the leaf upward.
	for i := len(segments) - 1; i >= 0; i-- {
		if len(node.keys) > 0 || len(node.children) > 0 {
			break
		}
		parent := parents[i]
		delete(parent.children, segments[i])
		node = parent
	}
}

// getMatchingKeys returns the cache keys matching pattern. A trailing "*" matches
// the base path and all descendants; any other pattern matches only keys stored
// exactly at that path. An empty pattern is treated as the root.
func (pi *patternIndex) getMatchingKeys(pattern string) []string {
	if pattern == "" {
		pattern = rootPath
	}
	wildcard := strings.HasSuffix(pattern, wildcardChar)
	if wildcard {
		pattern = strings.TrimSuffix(pattern, wildcardChar)
	}
	segments := normalizePath(pattern)

	pi.mu.RLock()
	defer pi.mu.RUnlock()

	node := pi.root
	for _, seg := range segments {
		node = node.children[seg]
		if node == nil {
			return nil
		}
	}

	if wildcard {
		return appendSubtreeKeys(node, nil)
	}
	return directKeys(node)
}

// directKeys returns the keys stored exactly at node.
func directKeys(node *patternNode) []string {
	if len(node.keys) == 0 {
		return nil
	}
	keys := make([]string, 0, len(node.keys))
	for k := range node.keys {
		keys = append(keys, k)
	}
	return keys
}

// appendSubtreeKeys appends node's keys and all descendant keys to dst.
func appendSubtreeKeys(node *patternNode, dst []string) []string {
	for k := range node.keys {
		dst = append(dst, k)
	}
	for _, child := range node.children {
		dst = appendSubtreeKeys(child, dst)
	}
	return dst
}

// clear discards the entire index.
func (pi *patternIndex) clear() {
	pi.mu.Lock()
	pi.root = newPatternNode()
	pi.mu.Unlock()
}
