package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/unkn0wn-root/kioshun"
)

type User struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

func main() {
	fmt.Println("=== Advanced Cache Usage Example ===")

	config := kioshun.Config{
		MaxSize:         10000,
		ShardCount:      16,
		CleanupInterval: 1 * time.Minute,
		DefaultTTL:      30 * time.Minute,
		EvictionPolicy:  kioshun.LRU,
		StatsEnabled:    true,
	}

	userCache, err := kioshun.New[string, User](config)
	if err != nil {
		panic(err)
	}
	defer userCache.Close()

	fmt.Println("\n1. Operations with complex data types:")
	users := []User{
		{ID: "1", Name: "Alice Johnson", Email: "alice@example.com", CreatedAt: time.Now()},
		{ID: "2", Name: "Bob Smith", Email: "bob@example.com", CreatedAt: time.Now()},
		{ID: "3", Name: "Charlie Brown", Email: "charlie@example.com", CreatedAt: time.Now()},
	}

	for _, user := range users {
		userCache.Set(user.ID, user, time.Duration(30+len(user.Name))*time.Second)
	}
	userCache.Wait()

	fmt.Println("\n2. Concurrent access:")

	var wg sync.WaitGroup
	numWorkers := 10
	operationsPerWorker := 100

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			for j := 0; j < operationsPerWorker; j++ {
				key := fmt.Sprintf("user:%d:%d", workerID, j)
				user := User{
					ID:        key,
					Name:      fmt.Sprintf("User %d-%d", workerID, j),
					Email:     fmt.Sprintf("user%d_%d@example.com", workerID, j),
					CreatedAt: time.Now(),
				}

				switch j % 4 {
				case 0: // Set
					userCache.Set(key, user, 1*time.Hour)
				case 1: // Get
					if u, found := userCache.Get(key); found {
						_ = u.Name // Use the value
					}
				case 2: // GetWithTTL
					if u, ttl, found := userCache.GetWithTTL(key); found {
						_ = u.Name
						_ = ttl
					}
				case 3: // Exists
					userCache.Exists(key)
				}
			}
		}(i)
	}

	wg.Wait()
	userCache.Wait()
	fmt.Printf("Completed %d concurrent ops\n", numWorkers*operationsPerWorker)

	fmt.Println("\n3. Cache manager for multiple cache instances:")

	manager := kioshun.NewManager()
	defer manager.CloseAll()

	userConfig := kioshun.DefaultConfig()
	userConfig.MaxSize = 10000
	sessionConfig := kioshun.DefaultConfig()
	sessionConfig.MaxSize = 50000
	sessionConfig.DefaultTTL = 2 * time.Hour
	apiConfig := kioshun.DefaultConfig()
	apiConfig.MaxSize = 100000
	apiConfig.DefaultTTL = 15 * time.Minute

	if err := manager.Register("users", userConfig); err != nil {
		panic(err)
	}
	if err := manager.Register("sessions", sessionConfig); err != nil {
		panic(err)
	}
	if err := manager.Register("api_responses", apiConfig); err != nil {
		panic(err)
	}

	userManagedCache, err := kioshun.GetCache[string, User](manager, "users")
	if err != nil {
		panic(err)
	}
	sessionCache, err := kioshun.GetCache[string, string](manager, "sessions")
	if err != nil {
		panic(err)
	}
	apiCache, err := kioshun.GetCache[string, []byte](manager, "api_responses")
	if err != nil {
		panic(err)
	}

	userManagedCache.Set("managed_user", users[0], 1*time.Hour)
	sessionCache.Set("session_123", "user_session_token", 2*time.Hour)
	apiCache.Set("api_response_1", []byte(`{"status": "success"}`), 15*time.Minute)

	fmt.Println("\n4. Global cache usage:")

	if err := kioshun.RegisterGlobalCache("global_users", userConfig); err != nil {
		panic(err)
	}
	if err := kioshun.RegisterGlobalCache("global_sessions", sessionConfig); err != nil {
		panic(err)
	}

	globalUserCache, err := kioshun.GetGlobalCache[string, User]("global_users")
	if err != nil {
		panic(err)
	}
	globalSessionCache, err := kioshun.GetGlobalCache[string, string]("global_sessions")
	if err != nil {
		panic(err)
	}

	globalUserCache.Set("global_user_1", users[0], 1*time.Hour)
	globalSessionCache.Set("global_session_1", "global_token", 2*time.Hour)

	fmt.Println("\n5. Performance monitoring:")

	// Generate some activity
	for i := 0; i < 1000; i++ {
		key := fmt.Sprintf("perf_test_%d", i)
		userCache.Set(key, users[i%len(users)], 1*time.Hour)

		// Mix reads and writes
		if i%3 == 0 {
			userCache.Get(key)
		}
	}
	userCache.Wait()

	stats := userCache.Stats()
	fmt.Printf("Performance Statistics:\n")
	fmt.Printf("  Total Operations: %d\n", stats.Hits+stats.Misses)
	fmt.Printf("  Hits: %d\n", stats.Hits)
	fmt.Printf("  Misses: %d\n", stats.Misses)
	fmt.Printf("  Hit Ratio: %.2f%%\n", stats.HitRatio*100)
	fmt.Printf("  Evictions: %d\n", stats.Evictions)
	fmt.Printf("  Expirations: %d\n", stats.Expirations)
	fmt.Printf("  Current Size: %d\n", stats.Size)
	fmt.Printf("  Max Capacity: %d\n", stats.Capacity)
	fmt.Printf("  Shards: %d\n", stats.Shards)

	fmt.Println("\n6. TTL and expiration handling:")

	// short TTL
	shortTTLCache := kioshun.NewDefault[string, string]()
	defer shortTTLCache.Close()

	shortTTLCache.Set("short_lived_1", "expires_soon", 1*time.Second)
	shortTTLCache.Set("short_lived_2", "expires_later", 3*time.Second)
	shortTTLCache.Wait()

	fmt.Printf("Initial size: %d\n", shortTTLCache.Size())

	time.Sleep(2 * time.Second)
	fmt.Printf("After 2 seconds: %d\n", shortTTLCache.Size())

	if _, found := shortTTLCache.Get("short_lived_1"); !found {
		fmt.Println("short_lived_1 has expired")
	}
	if _, found := shortTTLCache.Get("short_lived_2"); found {
		fmt.Println("short_lived_2 still exists")
	}

	fmt.Println("\n7. Manual cleanup:")

	userCache.TriggerCleanup()
	fmt.Println("Manual cleanup triggered")

	fmt.Println("\n8. Batch operations:")

	batchCache := kioshun.NewDefault[string, string]()
	defer batchCache.Close()

	// simulate batch insert
	start := time.Now()
	for i := 0; i < 10000; i++ {
		batchCache.Set(fmt.Sprintf("batch_key_%d", i), fmt.Sprintf("batch_value_%d", i), 1*time.Hour)
	}
	insertDuration := time.Since(start)
	batchCache.Wait()

	// batch read
	start = time.Now()
	for i := 0; i < 10000; i++ {
		batchCache.Get(fmt.Sprintf("batch_key_%d", i))
	}
	readDuration := time.Since(start)

	fmt.Printf("Batch insert (10,000 items): %v\n", insertDuration)
	fmt.Printf("Batch read (10,000 items): %v\n", readDuration)
	fmt.Printf("Insert rate: %.0f ops/sec\n", 10000/insertDuration.Seconds())
	fmt.Printf("Read rate: %.0f ops/sec\n", 10000/readDuration.Seconds())

	fmt.Println("\n=== Example completed ===")
}
