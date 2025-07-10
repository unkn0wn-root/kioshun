package cache

import (
	"strings"
	"sync"
)

type patternNode struct {
	children map[string]*patternNode
	keys     map[string]bool
	isWild   bool
}

type patternIndex struct {
	mu   sync.RWMutex
	root *patternNode
}

func newPatternIndex() *patternIndex {
	return &patternIndex{
		root: &patternNode{
			children: make(map[string]*patternNode),
			keys:     make(map[string]bool),
		},
	}
}

func (pi *patternIndex) addKey(path, key string) {
	pi.mu.Lock()
	defer pi.mu.Unlock()

	if path == "" {
		path = "/"
	}

	segments := strings.Split(strings.Trim(path, "/"), "/")
	if len(segments) == 1 && segments[0] == "" {
		segments = []string{}
	}

	node := pi.root
	for _, segment := range segments {
		if node.children[segment] == nil {
			node.children[segment] = &patternNode{
				children: make(map[string]*patternNode),
				keys:     make(map[string]bool),
			}
		}
		node = node.children[segment]
	}
	node.keys[key] = true
}

func (pi *patternIndex) removeKey(path, key string) {
	pi.mu.Lock()
	defer pi.mu.Unlock()

	if path == "" {
		path = "/"
	}

	segments := strings.Split(strings.Trim(path, "/"), "/")
	if len(segments) == 1 && segments[0] == "" {
		segments = []string{}
	}

	node := pi.root
	for _, segment := range segments {
		if node.children[segment] == nil {
			return
		}
		node = node.children[segment]
	}
	delete(node.keys, key)
}

func (pi *patternIndex) getMatchingKeys(pattern string) []string {
	pi.mu.RLock()
	defer pi.mu.RUnlock()

	if pattern == "" {
		pattern = "/"
	}

	isWildcard := strings.HasSuffix(pattern, "*")
	if isWildcard {
		pattern = strings.TrimSuffix(pattern, "*")
	}

	segments := strings.Split(strings.Trim(pattern, "/"), "/")
	if len(segments) == 1 && segments[0] == "" {
		segments = []string{}
	}

	node := pi.root
	for _, segment := range segments {
		if node.children[segment] == nil {
			return nil
		}
		node = node.children[segment]
	}

	var keys []string
	if isWildcard {
		keys = pi.collectAllKeys(node)
	} else {
		for key := range node.keys {
			keys = append(keys, key)
		}
	}

	return keys
}

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

func (pi *patternIndex) clear() {
	pi.mu.Lock()
	defer pi.mu.Unlock()

	pi.root = &patternNode{
		children: make(map[string]*patternNode),
		keys:     make(map[string]bool),
	}
}
