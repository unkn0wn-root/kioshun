package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	cache "github.com/unkn0wn-root/kioshun"
)

type TestAPI struct {
	cache      *cache.InMemoryCache[string, *APIResponse]
	middleware *cache.HTTPCacheMiddleware
	stats      *PerformanceStats
	server     *http.Server
}

type APIResponse struct {
	ID        string            `json:"id"`
	Data      map[string]string `json:"data"`
	Timestamp time.Time         `json:"timestamp"`
	Heavy     []byte            `json:"heavy,omitempty"`
}

type PerformanceStats struct {
	RequestCount    int64     `json:"request_count"`
	CacheHits       int64     `json:"cache_hits"`
	CacheMisses     int64     `json:"cache_misses"`
	EvictionCount   int64     `json:"eviction_count"`
	AvgResponseTime float64   `json:"avg_response_ms"`
	StartTime       time.Time `json:"start_time"`
	mu              sync.RWMutex
	responseTimes   []time.Duration
}

func NewTestAPI(evictionPolicy cache.EvictionPolicy) *TestAPI {
	config := cache.Config{
		MaxSize:         50000,
		ShardCount:      32,
		CleanupInterval: 30 * time.Second,
		DefaultTTL:      5 * time.Minute,
		EvictionPolicy:  evictionPolicy,
		StatsEnabled:    true,
	}

	if evictionPolicy == cache.SampledLFU {
		config.AdmissionResetInterval = 30 * time.Second
	}

	cacheInstance := cache.New[string, *APIResponse](config)

	middlewareConfig := cache.MiddlewareConfig{
		MaxSize:                50000,
		ShardCount:             32,
		CleanupInterval:        30 * time.Second,
		DefaultTTL:             5 * time.Minute,
		EvictionPolicy:         evictionPolicy,
		AdmissionResetInterval: 30 * time.Second,
		StatsEnabled:           true,
		CacheableMethods:       []string{"GET"},
		CacheableStatus:        []int{200},
		MaxBodySize:            1024 * 1024,
		HitHeader:              "X-Cache-Status",
		MissHeader:             "X-Cache-Status",
	}

	middleware := cache.NewHTTPCacheMiddleware(middlewareConfig)

	stats := &PerformanceStats{
		StartTime:     time.Now(),
		responseTimes: make([]time.Duration, 0, 10000),
	}

	api := &TestAPI{
		cache:      cacheInstance,
		middleware: middleware,
		stats:      stats,
	}

	return api
}

func (api *TestAPI) setupRoutes() *http.ServeMux {
	mux := http.NewServeMux()

	mux.Handle("/api/data/", api.performanceMiddleware(api.middleware.Middleware(http.HandlerFunc(api.handleDataRequest))))
	mux.Handle("/api/heavy/", api.performanceMiddleware(api.middleware.Middleware(http.HandlerFunc(api.handleHeavyRequest))))
	mux.Handle("/api/random/", api.performanceMiddleware(api.middleware.Middleware(http.HandlerFunc(api.handleRandomRequest))))
	mux.Handle("/api/popular/", api.performanceMiddleware(api.middleware.Middleware(http.HandlerFunc(api.handlePopularRequest))))

	mux.HandleFunc("/stats", api.handleStats)
	mux.HandleFunc("/cache-stats", api.handleCacheStats)
	mux.HandleFunc("/health", api.handleHealth)
	mux.HandleFunc("/load-test", api.handleLoadTest)
	mux.HandleFunc("/clear-cache", api.handleClearCache)

	return mux
}

func (api *TestAPI) performanceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		atomic.AddInt64(&api.stats.RequestCount, 1)

		next.ServeHTTP(w, r)

		duration := time.Since(start)
		api.stats.mu.Lock()
		api.stats.responseTimes = append(api.stats.responseTimes, duration)
		if len(api.stats.responseTimes) > 10000 {
			api.stats.responseTimes = api.stats.responseTimes[5000:]
		}
		api.stats.mu.Unlock()

		if cacheStatus := w.Header().Get("X-Cache-Status"); cacheStatus == "HIT" {
			atomic.AddInt64(&api.stats.CacheHits, 1)
		} else if cacheStatus == "MISS" {
			atomic.AddInt64(&api.stats.CacheMisses, 1)
		}
	})
}

func (api *TestAPI) handleDataRequest(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/api/data/"):]
	if id == "" {
		http.Error(w, "ID required", http.StatusBadRequest)
		return
	}

	response := &APIResponse{
		ID:        id,
		Data:      generateSampleData(id),
		Timestamp: time.Now(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (api *TestAPI) handleHeavyRequest(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/api/heavy/"):]
	if id == "" {
		http.Error(w, "ID required", http.StatusBadRequest)
		return
	}

	time.Sleep(time.Millisecond * 50)

	response := &APIResponse{
		ID:        id,
		Data:      generateSampleData(id),
		Heavy:     generateHeavyData(10240), // 10KB payload
		Timestamp: time.Now(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (api *TestAPI) handleRandomRequest(w http.ResponseWriter, r *http.Request) {
	id := fmt.Sprintf("random_%d", rand.Int63())

	response := &APIResponse{
		ID:        id,
		Data:      generateSampleData(id),
		Timestamp: time.Now(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (api *TestAPI) handlePopularRequest(w http.ResponseWriter, r *http.Request) {
	popularIDs := []string{"pop1", "pop2", "pop3", "pop4", "pop5"}
	weights := []int{50, 25, 15, 7, 3} // Zipfian-like distribution

	totalWeight := 100
	rand := rand.Intn(totalWeight)

	var selectedID string
	cumWeight := 0
	for i, weight := range weights {
		cumWeight += weight
		if rand < cumWeight {
			selectedID = popularIDs[i]
			break
		}
	}

	response := &APIResponse{
		ID:        selectedID,
		Data:      generateSampleData(selectedID),
		Timestamp: time.Now(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (api *TestAPI) handleStats(w http.ResponseWriter, r *http.Request) {
	api.stats.mu.RLock()
	responseTimes := make([]time.Duration, len(api.stats.responseTimes))
	copy(responseTimes, api.stats.responseTimes)
	api.stats.mu.RUnlock()

	var totalTime time.Duration
	for _, rt := range responseTimes {
		totalTime += rt
	}

	avgTime := float64(0)
	if len(responseTimes) > 0 {
		avgTime = float64(totalTime.Nanoseconds()) / float64(len(responseTimes)) / 1e6
	}

	stats := struct {
		*PerformanceStats
		AvgResponseTime float64 `json:"avg_response_ms"`
		Uptime          string  `json:"uptime"`
		CacheHitRatio   string  `json:"cache_hit_ratio"`
	}{
		PerformanceStats: api.stats,
		AvgResponseTime:  avgTime,
		Uptime:           time.Since(api.stats.StartTime).String(),
	}

	totalCacheOps := atomic.LoadInt64(&api.stats.CacheHits) + atomic.LoadInt64(&api.stats.CacheMisses)
	if totalCacheOps > 0 {
		hitRatio := float64(atomic.LoadInt64(&api.stats.CacheHits)) / float64(totalCacheOps) * 100
		stats.CacheHitRatio = fmt.Sprintf("%.2f%%", hitRatio)
	} else {
		stats.CacheHitRatio = "N/A"
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

func (api *TestAPI) handleCacheStats(w http.ResponseWriter, r *http.Request) {
	middlewareStats := api.middleware.Stats()
	cacheStats := api.cache.Stats()

	combined := struct {
		Middleware cache.Stats `json:"middleware"`
		Direct     cache.Stats `json:"direct_cache"`
		Runtime    struct {
			NumGoroutine int    `json:"goroutines"`
			MemStats     string `json:"memory_mb"`
		} `json:"runtime"`
	}{
		Middleware: middlewareStats,
		Direct:     cacheStats,
	}

	combined.Runtime.NumGoroutine = runtime.NumGoroutine()

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	combined.Runtime.MemStats = fmt.Sprintf("%.2f", float64(mem.Alloc)/1024/1024)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(combined)
}

func (api *TestAPI) handleHealth(w http.ResponseWriter, r *http.Request) {
	health := struct {
		Status    string `json:"status"`
		Timestamp string `json:"timestamp"`
		Version   string `json:"version"`
	}{
		Status:    "healthy",
		Timestamp: time.Now().Format(time.RFC3339),
		Version:   "1.0.0",
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(health)
}

func (api *TestAPI) handleLoadTest(w http.ResponseWriter, r *http.Request) {
	durationStr := r.URL.Query().Get("duration")
	concurrencyStr := r.URL.Query().Get("concurrency")

	duration := 30 * time.Second
	if durationStr != "" {
		if d, err := time.ParseDuration(durationStr); err == nil {
			duration = d
		}
	}

	concurrency := 10
	if concurrencyStr != "" {
		if c, err := strconv.Atoi(concurrencyStr); err == nil && c > 0 && c <= 10000 {
			concurrency = c
		}
	}

	result := api.runLoadTest(duration, concurrency, r.Host)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func (api *TestAPI) handleClearCache(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	api.middleware.Clear()
	api.cache.Clear()

	response := struct {
		Message   string `json:"message"`
		Timestamp string `json:"timestamp"`
	}{
		Message:   "Cache cleared successfully",
		Timestamp: time.Now().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (api *TestAPI) runLoadTest(duration time.Duration, concurrency int, port string) map[string]interface{} {
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	var wg sync.WaitGroup
	var requests int64
	var errors int64
	var totalResponseTime int64

	endpoints := []string{
		"/api/data/item_%d",
		"/api/heavy/heavy_%d",
		"/api/popular/",
		"/api/random/",
	}

	transport := &http.Transport{
		MaxIdleConns:        concurrency * 2,
		MaxIdleConnsPerHost: concurrency,
		IdleConnTimeout:     90 * time.Second,
		DisableKeepAlives:   false,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}

	startTime := time.Now()

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			for {
				select {
				case <-ctx.Done():
					return
				default:
					requestStart := time.Now()
					endpointIndex := rand.Intn(len(endpoints))
					endpoint := endpoints[endpointIndex]

					var url string
					if endpointIndex < 2 { // data and heavy endpoints need ID
						url = fmt.Sprintf("http://"+port+endpoint, rand.Intn(1000))
					} else { // popular and random endpoints don't need ID
						url = "http://" + port + endpoint
					}

					resp, err := client.Get(url)
					requestDuration := time.Since(requestStart)
					atomic.AddInt64(&requests, 1)
					atomic.AddInt64(&totalResponseTime, requestDuration.Nanoseconds())

					if err != nil {
						atomic.AddInt64(&errors, 1)
						continue
					}

					if resp != nil {
						resp.Body.Close()
						if resp.StatusCode != 200 {
							atomic.AddInt64(&errors, 1)
						}
					}
				}
			}
		}(i)
	}

	wg.Wait()
	actualDuration := time.Since(startTime)

	totalReqs := atomic.LoadInt64(&requests)
	avgResponseTimeMs := float64(0)
	if totalReqs > 0 {
		avgResponseTimeMs = float64(atomic.LoadInt64(&totalResponseTime)) / float64(totalReqs) / 1e6
	}

	return map[string]interface{}{
		"duration_seconds":     actualDuration.Seconds(),
		"total_requests":       totalReqs,
		"error_count":          atomic.LoadInt64(&errors),
		"requests_per_second":  float64(totalReqs) / actualDuration.Seconds(),
		"concurrency":          concurrency,
		"error_rate":           float64(atomic.LoadInt64(&errors)) / float64(totalReqs) * 100,
		"avg_response_time_ms": avgResponseTimeMs,
	}
}

func generateSampleData(id string) map[string]string {
	return map[string]string{
		"id":          id,
		"type":        "sample",
		"description": fmt.Sprintf("Sample data for %s", id),
		"metadata":    fmt.Sprintf("meta_%d", time.Now().Unix()),
		"category":    []string{"A", "B", "C", "D", "E"}[len(id)%5],
	}
}

func generateHeavyData(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(rand.Intn(256))
	}
	return data
}

func (api *TestAPI) Start(port string, evictionPolicy cache.EvictionPolicy) error {
	mux := api.setupRoutes()

	api.server = &http.Server{
		Addr:           ":" + port,
		Handler:        mux,
		ReadTimeout:    60 * time.Second,
		WriteTimeout:   60 * time.Second,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	cacheStats := api.cache.Stats()
	log.Printf("Cache Test API starting on port %s", port)
	log.Printf("Using %s eviction policy with %d shards", getEvictionPolicyName(evictionPolicy), cacheStats.Shards)
	log.Printf("Maximum cache size: 50,000 items")
	if evictionPolicy == cache.SampledLFU {
		log.Printf("Admission filter reset interval: 30s")
	}
	log.Printf("Configured for load testing")
	log.Println()
	log.Println("Available endpoints:")
	log.Println("  GET  /api/data/{id}     - Cached data requests")
	log.Println("  GET  /api/heavy/{id}    - Heavy computation requests")
	log.Println("  GET  /api/popular/      - Popular content simulation")
	log.Println("  GET  /api/random/       - Random content requests")
	log.Println("  GET  /stats             - Performance statistics")
	log.Println("  GET  /cache-stats       - Cache internals")
	log.Println("  GET  /health            - Health check")
	log.Println("  GET  /load-test         - Built-in load testing")
	log.Println("  POST /clear-cache       - Clear all caches")
	log.Println()

	return api.server.ListenAndServe()
}

func (api *TestAPI) Stop() error {
	if api.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		api.server.Shutdown(ctx)
	}

	if api.cache != nil {
		api.cache.Close()
	}

	if api.middleware != nil {
		api.middleware.Close()
	}

	return nil
}

func parseEvictionPolicy(policy string) cache.EvictionPolicy {
	switch strings.ToLower(policy) {
	case "lru":
		return cache.LRU
	case "lfu":
		return cache.LFU
	case "fifo":
		return cache.FIFO
	case "random":
		return cache.Random
	case "sampledlfu":
		return cache.SampledLFU
	default:
		log.Printf("Unknown eviction policy '%s', using LRU", policy)
		return cache.LRU
	}
}

func getEvictionPolicyName(policy cache.EvictionPolicy) string {
	switch policy {
	case cache.LRU:
		return "LRU"
	case cache.LFU:
		return "LFU"
	case cache.FIFO:
		return "FIFO"
	case cache.Random:
		return "Random"
	case cache.SampledLFU:
		return "SampledLFU"
	default:
		return "Unknown"
	}
}

func main() {
	var evictionPolicyStr = flag.String("eviction", "lru", "Eviction policy: lru, lfu, fifo, random, sampledlfu")
	var port = flag.String("port", "8080", "Port to listen on")
	flag.Parse()

	evictionPolicy := parseEvictionPolicy(*evictionPolicyStr)

	log.Printf("Starting Cache Test API with %s eviction policy", getEvictionPolicyName(evictionPolicy))

	api := NewTestAPI(evictionPolicy)
	defer api.Stop()

	if err := api.Start(*port, evictionPolicy); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Failed to start server: %v", err)
	}
}
