package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/david0/cago"
)

type APIResponse struct {
	Message   string    `json:"message"`
	Timestamp time.Time `json:"timestamp"`
	Data      any       `json:"data,omitempty"`
}

type User struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

func main() {
	fmt.Println("=== HTTP Cache Middleware Example ===")

	basicConfig := cache.DefaultMiddlewareConfig()
	basicConfig.DefaultTTL = 2 * time.Minute
	basicMiddleware := cache.NewHTTPCacheMiddleware(basicConfig)
	defer basicMiddleware.Close()

	apiConfig := cache.DefaultMiddlewareConfig()
	apiConfig.MaxSize = 50000
	apiConfig.ShardCount = 32
	apiConfig.DefaultTTL = 5 * time.Minute
	apiConfig.MaxBodySize = 5 * 1024 * 1024 // 5MB
	apiMiddleware := cache.NewHTTPCacheMiddleware(apiConfig)
	defer apiMiddleware.Close()

	userConfig := cache.DefaultMiddlewareConfig()
	userConfig.DefaultTTL = 10 * time.Minute
	userMiddleware := cache.NewHTTPCacheMiddleware(userConfig)
	userMiddleware.SetKeyGenerator(cache.KeyWithUserID("X-User-ID"))
	defer userMiddleware.Close()

	contentConfig := cache.DefaultMiddlewareConfig()
	contentMiddleware := cache.NewHTTPCacheMiddleware(contentConfig)
	contentMiddleware.SetCachePolicy(cache.CacheByContentType(map[string]time.Duration{
		"application/json": 5 * time.Minute,
		"text/html":       10 * time.Minute,
		"image/":          1 * time.Hour,
	}, 2*time.Minute))
	defer contentMiddleware.Close()

	// size-based conditional caching
	conditionalConfig := cache.DefaultMiddlewareConfig()
	conditionalMiddleware := cache.NewHTTPCacheMiddleware(conditionalConfig)
	conditionalMiddleware.SetCachePolicy(cache.CacheBySize(100, 1024*1024, 3*time.Minute))
	defer conditionalMiddleware.Close()

	basicMiddleware.OnHit(func(key string) {
		fmt.Printf("🎯 BASIC CACHE HIT: %s\n", key[:8])
	})
	basicMiddleware.OnMiss(func(key string) {
		fmt.Printf("❌ BASIC CACHE MISS: %s\n", key[:8])
	})

	apiMiddleware.OnHit(func(key string) {
		fmt.Printf("🚀 API CACHE HIT: %s\n", key[:8])
	})

	userMiddleware.OnHit(func(key string) {
		fmt.Printf("👤 USER CACHE HIT: %s\n", key[:8])
	})

	users := []User{
		{ID: "1", Name: "Alina Malina", Email: "alice@example.com"},
		{ID: "2", Name: "Bob Knob", Email: "bob@example.com"},
		{ID: "3", Name: "Charlie Sterone", Email: "charlie@example.com"},
	}

	// CACHING ENDPOINTS
	http.Handle("/basic/users", basicMiddleware.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond) // simulate database query

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=180") // 3 minutes

		json.NewEncoder(w).Encode(APIResponse{
			Message:   "Users retrieved successfully",
			Timestamp: time.Now(),
			Data:      users,
		})
	})))

	http.Handle("/basic/user/", basicMiddleware.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID := strings.TrimPrefix(r.URL.Path, "/basic/user/")
		time.Sleep(50 * time.Millisecond)

		w.Header().Set("Content-Type", "application/json")

		for _, user := range users {
			if user.ID == userID {
				json.NewEncoder(w).Encode(APIResponse{
					Message:   "User found",
					Timestamp: time.Now(),
					Data:      user,
				})
				return
			}
		}

		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(APIResponse{
			Message: "User not found",
			Timestamp: time.Now(),
		})
	})))

	// API ENDPOINTS
	http.Handle("/api/products", apiMiddleware.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond) // simulate complex database query

		products := []map[string]interface{}{
			{"id": 1, "name": "Laptop Super Pro", "price": 1299.99, "category": "Electronics"},
			{"id": 2, "name": "Wireless Flying Board", "price": 29.99, "category": "Electronics"},
			{"id": 3, "name": "Coffee Machine But for Tea", "price": 12.99, "category": "Kitchen"},
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=300") // 5 minutes

		json.NewEncoder(w).Encode(APIResponse{
			Message:   "Products retrieved",
			Timestamp: time.Now(),
			Data:      products,
		})
	})))

	http.Handle("/api/search", apiMiddleware.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("q")
		if query == "" {
			query = "all"
		}

		time.Sleep(300 * time.Millisecond) // simulate search operation

		results := []string{
			fmt.Sprintf("result-1-for-%s", query),
			fmt.Sprintf("result-2-for-%s", query),
			fmt.Sprintf("result-3-for-%s", query),
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(APIResponse{
			Message:   fmt.Sprintf("Search results for '%s'", query),
			Timestamp: time.Now(),
			Data:      results,
		})
	})))

	// USER-SPECIFIC CACHING
	http.Handle("/user/profile", userMiddleware.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID := r.Header.Get("X-User-ID")
		if userID == "" {
			userID = "anonymous"
		}

		time.Sleep(150 * time.Millisecond) // simulate user data retrieval

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(APIResponse{
			Message:   fmt.Sprintf("Profile for user %s", userID),
			Timestamp: time.Now(),
			Data: map[string]string{
				"user_id":     userID,
				"name":        "User " + userID,
				"preferences": "theme=dark,lang=en",
				"last_login":  time.Now().Add(-2 * time.Hour).Format(time.RFC3339),
			},
		})
	})))

	// CONTENT-TYPE BASED CACHING
	http.Handle("/content/json", contentMiddleware.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"type": "json",
			"ttl":  "5 minutes",
			"time": time.Now(),
		})
	})))

	http.Handle("/content/html", contentMiddleware.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.Header().Set("Content-Type", "text/html")
		html := fmt.Sprintf(`<html><body><h1>HTML Content</h1><p>TTL: 10 minutes</p><p>Time: %s</p></body></html>`, time.Now().Format(time.RFC3339))
		w.Write([]byte(html))
	})))

	http.Handle("/content/image", contentMiddleware.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("fake-png-data-cached-for-1-hour-" + time.Now().Format("15:04:05")))
	})))

	// SIZE-BASED CONDITIONAL CACHING
	http.Handle("/conditional/small", conditionalMiddleware.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"size":"small"}`)) // too small - won't be cached
	})))

	http.Handle("/conditional/large", conditionalMiddleware.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")

		// Large response - will be cached
		data := map[string]interface{}{
			"size":        "large",
			"description": "This response is large enough to be cached by our size-based policy",
			"timestamp":   time.Now(),
			"data":        make([]int, 50), // make it large enough
		}
		for i := range data["data"].([]int) {
			data["data"].([]int)[i] = i
		}
		json.NewEncoder(w).Encode(data)
	})))

	// CACHE MANAGEMENT AND STATISTICS
	http.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		stats := map[string]interface{}{
			"basic":       basicMiddleware.Stats(),
			"api":         apiMiddleware.Stats(),
			"user":        userMiddleware.Stats(),
			"content":     contentMiddleware.Stats(),
			"conditional": conditionalMiddleware.Stats(),
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(stats)
	})

	http.HandleFunc("/cache/clear", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		cacheType := r.URL.Query().Get("type")
		cleared := 0

		switch cacheType {
		case "basic":
			basicMiddleware.Clear()
			cleared = 1
		case "api":
			apiMiddleware.Clear()
			cleared = 1
		case "user":
			userMiddleware.Clear()
			cleared = 1
		case "content":
			contentMiddleware.Clear()
			cleared = 1
		case "conditional":
			conditionalMiddleware.Clear()
			cleared = 1
		default:
			basicMiddleware.Clear()
			apiMiddleware.Clear()
			userMiddleware.Clear()
			contentMiddleware.Clear()
			conditionalMiddleware.Clear()
			cleared = 5
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(APIResponse{
			Message:   fmt.Sprintf("Cleared %d cache(s)", cleared),
			Timestamp: time.Now(),
			Data:      map[string]string{"type": cacheType},
		})
	})

	// UNCACHED ENDPOINT FOR COMPARISON
	http.HandleFunc("/uncached/time", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache") // explicitly no-cache

		json.NewEncoder(w).Encode(APIResponse{
			Message:   "Current time (never cached)",
			Timestamp: time.Now(),
		})
	})

	// Start server
	fmt.Println("\nServer starting on :8080")
	fmt.Println("\nBasic Caching (2min TTL):")
	fmt.Println("  curl http://localhost:8080/basic/users")
	fmt.Println("  curl http://localhost:8080/basic/user/1")

	fmt.Println("\n🚀 API Caching (5min TTL, 32 shards):")
	fmt.Println("  curl http://localhost:8080/api/products")
	fmt.Println("  curl http://localhost:8080/api/search?q=laptop")

	fmt.Println("\n👤 User-Specific Caching (10min TTL):")
	fmt.Println("  curl -H 'X-User-ID: 123' http://localhost:8080/user/profile")
	fmt.Println("  curl -H 'X-User-ID: 456' http://localhost:8080/user/profile")

	fmt.Println("\n🎯 Content-Type Based Caching:")
	fmt.Println("  curl http://localhost:8080/content/json   # 5min TTL")
	fmt.Println("  curl http://localhost:8080/content/html   # 10min TTL")
	fmt.Println("  curl http://localhost:8080/content/image  # 1hour TTL")

	fmt.Println("\n🔍 Size-Based Conditional Caching:")
	fmt.Println("  curl http://localhost:8080/conditional/small  # Not cached (too small)")
	fmt.Println("  curl http://localhost:8080/conditional/large  # Cached (large enough)")

	fmt.Println("\n📊 Management & Statistics:")
	fmt.Println("  curl http://localhost:8080/stats")
	fmt.Println("  curl -X POST http://localhost:8080/cache/clear")
	fmt.Println("  curl -X POST http://localhost:8080/cache/clear?type=api")

	fmt.Println("\n🔄 Comparison:")
	fmt.Println("  curl http://localhost:8080/uncached/time  # Never cached")

	fmt.Println("\n💡 Watch the console for cache hit/miss notifications")
	fmt.Println("💡 You can call endpoints multiple times to see caching in action")
	fmt.Println("💡 Check X-Cache headers: HIT/MISS, X-Cache-Date, X-Cache-Age")

	log.Fatal(http.ListenAndServe(":8080", nil))
}
