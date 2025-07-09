package main

import (
	"fmt"
	"time"

	"github.com/unkn0wn-root/kioshun"
)

func main() {
	fmt.Println("=== Basic Cache Usage ===")

	// Create a cache with default configuration
	cache := cache.NewWithDefaults[string, string]()
	defer cache.Close()

	// Basic Set and Get operations
	fmt.Println("\n1. Basic Set and Get ops:")
	cache.Set("user:123", "David Kier", 5*time.Minute)
	cache.Set("user:456", "Michael Ballack", 5*time.Minute)
	cache.Set("user:789", "Cristiano Bombaldo", 5*time.Minute)

	if value, found := cache.Get("user:123"); found {
		fmt.Printf("Found user: %s\n", value)
	}

	// Get with TTL information
	fmt.Println("\n2. Get with TTL:")
	if value, ttl, found := cache.GetWithTTL("user:123"); found {
		fmt.Printf("User: %s, TTL remaining: %s\n", value, ttl)
	}

	// Check existence without updating access time
	fmt.Println("\n3. Check existence:")
	if cache.Exists("user:123") {
		fmt.Println("User 123 exists in cache")
	}

	// Delete operation
	fmt.Println("\n4. Delete ops:")
	if cache.Delete("user:456") {
		fmt.Println("User 456 deleted from cache")
	}

	// Check size
	fmt.Println("\n5. Cache size:")
	fmt.Printf("Current cache size: %d\n", cache.Size())

	// Get all keys
	fmt.Println("\n6. All keys:")
	keys := cache.Keys()
	for _, key := range keys {
		fmt.Printf("Key: %s\n", key)
	}

	// Cache statistics
	fmt.Println("\n7. Cache statistics:")
	stats := cache.Stats()
	fmt.Printf("Hits: %d, Misses: %d, Size: %d, Hit Ratio: %.2f%%\n",
		stats.Hits, stats.Misses, stats.Size, stats.HitRatio*100)

	// Set with callback on expiration
	fmt.Println("\n8. Set with expiration callback:")
	cache.SetWithCallback("temp:data", "temporary value", 2*time.Second, func(key string, value string) {
		fmt.Printf("Key %s expired with value: %s\n", key, value)
	})

	// Wait for expiration
	time.Sleep(3 * time.Second)

	// Clear all
	fmt.Println("\n9. Clear all:")
	cache.Clear()
	fmt.Printf("Cache size after clear: %d\n", cache.Size())

	fmt.Println("\n=== Example completed ===")
}
