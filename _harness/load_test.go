package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type LoadTestScenario struct {
	Name           string
	Duration       time.Duration
	Concurrency    int
	RequestPattern RequestPattern
	Description    string
	BestFor        []string
}

type RequestPattern func(clientID int, requestNum int64) string

type LoadTestResult struct {
	Scenario        string                 `json:"scenario"`
	Duration        time.Duration          `json:"duration"`
	TotalRequests   int64                  `json:"total_requests"`
	SuccessRequests int64                  `json:"success_requests"`
	ErrorRequests   int64                  `json:"error_requests"`
	RequestsPerSec  float64                `json:"requests_per_second"`
	AvgLatency      time.Duration          `json:"avg_latency_ms"`
	P95Latency      time.Duration          `json:"p95_latency_ms"`
	P99Latency      time.Duration          `json:"p99_latency_ms"`
	CacheStats      CacheTestStats         `json:"cache_stats"`
	MemoryUsage     map[string]interface{} `json:"memory_usage"`
	ErrorRate       float64                `json:"error_rate_percent"`
}

type CacheTestStats struct {
	Hits        int64   `json:"hits"`
	Misses      int64   `json:"misses"`
	HitRatio    float64 `json:"hit_ratio_percent"`
	Evictions   int64   `json:"evictions"`
	Size        int64   `json:"current_size"`
	Capacity    int64   `json:"max_capacity"`
	Utilization float64 `json:"utilization_percent"`
}

type LoadTester struct {
	baseURL string
	client  *http.Client
}

func NewLoadTester(baseURL string) *LoadTester {
	return &LoadTester{
		baseURL: baseURL,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (lt *LoadTester) GetTestScenarios() []LoadTestScenario {
	return []LoadTestScenario{
		{
			Name:        "Hot Spot Access",
			Duration:    60 * time.Second,
			Concurrency: 50,
			RequestPattern: func(clientID int, requestNum int64) string {
				// 80/20 rule: 80% of requests go to 20% of data
				if requestNum%10 < 8 {
					hotKey := requestNum % 20 // Hot keys: 0-19
					return fmt.Sprintf("/api/data/hot_%d", hotKey)
				}
				coldKey := requestNum%1000 + 100 // Cold keys: 100-1099
				return fmt.Sprintf("/api/data/cold_%d", coldKey)
			},
			Description: "Tests cache's ability to keep frequently accessed hot data",
			BestFor:     []string{"LRU", "LFU", "SampledLFU"},
		},
		{
			Name:        "Cache Pollution Resistance",
			Duration:    45 * time.Second,
			Concurrency: 30,
			RequestPattern: func(clientID int, requestNum int64) string {
				if requestNum%5 == 0 {
					// 20% random keys (pollution)
					return fmt.Sprintf("/api/random/")
				}
				// 80% legitimate keys with some frequency
				return fmt.Sprintf("/api/data/legit_%d", requestNum%100)
			},
			Description: "Tests cache's resistance to random access pattern pollution",
			BestFor:     []string{"SampledLFU", "LFU"},
		},
		{
			Name:        "Heavy Computation Caching",
			Duration:    90 * time.Second,
			Concurrency: 20,
			RequestPattern: func(clientID int, requestNum int64) string {
				// with Zipfian
				weights := []int{40, 25, 15, 10, 5, 3, 2} // Decreasing popularity
				total := 100
				selector := int(requestNum) % total

				cumulative := 0
				for i, weight := range weights {
					cumulative += weight
					if selector < cumulative {
						return fmt.Sprintf("/api/heavy/compute_%d", i)
					}
				}
				return fmt.Sprintf("/api/heavy/compute_rare_%d", requestNum%50)
			},
			Description: "Tests caching effectiveness for expensive operations with Zipfian distribution",
			BestFor:     []string{"LRU", "LFU", "SampledLFU"},
		},
		{
			Name:        "Mixed Workload Stress",
			Duration:    120 * time.Second,
			Concurrency: 100,
			RequestPattern: func(clientID int, requestNum int64) string {
				patterns := []string{
					"/api/data/mixed_%d",
					"/api/heavy/stress_%d",
					"/api/popular/",
					"/api/random/",
				}

				// Different patterns based on client ID and request number
				patternIdx := (clientID + int(requestNum)) % len(patterns)
				if patternIdx < 2 {
					return fmt.Sprintf(patterns[patternIdx], requestNum%200)
				}
				return patterns[patternIdx]
			},
			Description: "Mixed workload to test overall performance and stability",
			BestFor:     []string{"LRU", "LFU", "FIFO", "Random", "SampledLFU"},
		},
		{
			Name:        "Burst Traffic Simulation",
			Duration:    60 * time.Second,
			Concurrency: 150,
			RequestPattern: func(clientID int, requestNum int64) string {
				// Simulate burst patterns - popular content during bursts
				cycle := requestNum / 100 // Every 100 requests is a cycle
				if cycle%3 == 0 {
					// Burst phase - everyone hits popular content
					return fmt.Sprintf("/api/data/trending_%d", requestNum%5)
				}
				// Normal phase - distributed access
				return fmt.Sprintf("/api/data/normal_%d", requestNum%500)
			},
			Description: "Tests cache behavior under bursty traffic patterns and trending content",
			BestFor:     []string{"LRU", "SampledLFU"},
		},
	}
}

func (lt *LoadTester) GetScenariosForPolicy(policyName string) []LoadTestScenario {
	allScenarios := lt.GetTestScenarios()
	var filtered []LoadTestScenario

	for _, scenario := range allScenarios {
		for _, bestPolicy := range scenario.BestFor {
			if bestPolicy == policyName || len(scenario.BestFor) >= 4 { // 4+ means good for most policies
				filtered = append(filtered, scenario)
				break
			}
		}
	}

	if len(filtered) == 0 {
		return allScenarios
	}

	return filtered
}

func (lt *LoadTester) RunScenario(scenario LoadTestScenario) (*LoadTestResult, error) {
	log.Printf("Starting scenario: %s", scenario.Name)
	log.Printf("%s", scenario.Description)
	log.Printf("Duration: %v, Concurrency: %d", scenario.Duration, scenario.Concurrency)

	// Clear cache before each test
	if err := lt.clearCache(); err != nil {
		log.Printf("- Warning: Could not clear cache: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), scenario.Duration)
	defer cancel()

	var (
		totalRequests   int64
		successRequests int64
		errorRequests   int64
		latencies       []time.Duration
		latenciesMux    sync.Mutex
	)

	startTime := time.Now()
	var wg sync.WaitGroup

	for i := 0; i < scenario.Concurrency; i++ {
		wg.Add(1)
		go func(clientID int) {
			defer wg.Done()

			var requestNum int64
			for {
				select {
				case <-ctx.Done():
					return
				default:
					url := lt.baseURL + scenario.RequestPattern(clientID, requestNum)

					reqStart := time.Now()
					resp, err := lt.client.Get(url)
					latency := time.Since(reqStart)

					atomic.AddInt64(&totalRequests, 1)
					requestNum++

					if err != nil {
						atomic.AddInt64(&errorRequests, 1)
						continue
					}

					resp.Body.Close()

					if resp.StatusCode == 200 {
						atomic.AddInt64(&successRequests, 1)
					} else {
						atomic.AddInt64(&errorRequests, 1)
					}

					// sample latencies (to avoid memory issues with very long tests)
					if requestNum%10 == 0 {
						latenciesMux.Lock()
						latencies = append(latencies, latency)
						// keep only recent latencies
						if len(latencies) > 10000 {
							latencies = latencies[5000:]
						}
						latenciesMux.Unlock()
					}
				}
			}
		}(i)
	}

	wg.Wait()
	actualDuration := time.Since(startTime)

	cacheStats, err := lt.getCacheStats()
	if err != nil {
		log.Printf("- Warning: Could not get cache stats: %v", err)
	}

	// Calculate latency percentiles
	latenciesMux.Lock()
	avgLatency, p95Latency, p99Latency := lt.calculateLatencyPercentiles(latencies)
	latenciesMux.Unlock()

	result := &LoadTestResult{
		Scenario:        scenario.Name,
		Duration:        actualDuration,
		TotalRequests:   atomic.LoadInt64(&totalRequests),
		SuccessRequests: atomic.LoadInt64(&successRequests),
		ErrorRequests:   atomic.LoadInt64(&errorRequests),
		RequestsPerSec:  float64(atomic.LoadInt64(&totalRequests)) / actualDuration.Seconds(),
		AvgLatency:      avgLatency,
		P95Latency:      p95Latency,
		P99Latency:      p99Latency,
		CacheStats:      cacheStats,
		ErrorRate:       float64(atomic.LoadInt64(&errorRequests)) / float64(atomic.LoadInt64(&totalRequests)) * 100,
	}

	if memStats, err := lt.getMemoryStats(); err == nil {
		result.MemoryUsage = memStats
	}

	lt.printScenarioResults(result)
	return result, nil
}

func (lt *LoadTester) calculateLatencyPercentiles(latencies []time.Duration) (avg, p95, p99 time.Duration) {
	if len(latencies) == 0 {
		return 0, 0, 0
	}

	// Sort latencies for percentile calculation
	for i := 0; i < len(latencies)-1; i++ {
		for j := 0; j < len(latencies)-i-1; j++ {
			if latencies[j] > latencies[j+1] {
				latencies[j], latencies[j+1] = latencies[j+1], latencies[j]
			}
		}
	}

	// average
	var total time.Duration
	for _, lat := range latencies {
		total += lat
	}
	avg = total / time.Duration(len(latencies))

	// percentiles
	p95Index := int(math.Ceil(0.95*float64(len(latencies)))) - 1
	p99Index := int(math.Ceil(0.99*float64(len(latencies)))) - 1

	if p95Index >= 0 && p95Index < len(latencies) {
		p95 = latencies[p95Index]
	}
	if p99Index >= 0 && p99Index < len(latencies) {
		p99 = latencies[p99Index]
	}

	return avg, p95, p99
}

func (lt *LoadTester) clearCache() error {
	req, err := http.NewRequest("POST", lt.baseURL+"/clear-cache", nil)
	if err != nil {
		return err
	}

	resp, err := lt.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("failed to clear cache: status %d", resp.StatusCode)
	}

	return nil
}

func (lt *LoadTester) getCacheStats() (CacheTestStats, error) {
	resp, err := lt.client.Get(lt.baseURL + "/cache-stats")
	if err != nil {
		return CacheTestStats{}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return CacheTestStats{}, err
	}

	var stats struct {
		Middleware struct {
			Hits      int64   `json:"hits"`
			Misses    int64   `json:"misses"`
			Evictions int64   `json:"evictions"`
			Size      int64   `json:"size"`
			Capacity  int64   `json:"capacity"`
			HitRatio  float64 `json:"hit_ratio"`
		} `json:"middleware"`
	}

	if err := json.Unmarshal(body, &stats); err != nil {
		return CacheTestStats{}, err
	}

	result := CacheTestStats{
		Hits:      stats.Middleware.Hits,
		Misses:    stats.Middleware.Misses,
		HitRatio:  stats.Middleware.HitRatio * 100,
		Evictions: stats.Middleware.Evictions,
		Size:      stats.Middleware.Size,
		Capacity:  stats.Middleware.Capacity,
	}

	if result.Capacity > 0 {
		result.Utilization = float64(result.Size) / float64(result.Capacity) * 100
	}

	return result, nil
}

func (lt *LoadTester) getMemoryStats() (map[string]interface{}, error) {
	resp, err := lt.client.Get(lt.baseURL + "/cache-stats")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var stats struct {
		Runtime struct {
			NumGoroutine int    `json:"goroutines"`
			MemStats     string `json:"memory_mb"`
		} `json:"runtime"`
	}

	if err := json.Unmarshal(body, &stats); err != nil {
		return nil, err
	}

	return map[string]interface{}{
		"goroutines": stats.Runtime.NumGoroutine,
		"memory_mb":  stats.Runtime.MemStats,
	}, nil
}

func (lt *LoadTester) printScenarioResults(result *LoadTestResult) {
	log.Printf("Scenario '%s' completed", result.Scenario)
	log.Printf("Results Summary:")
	log.Printf("   Total Requests: %d", result.TotalRequests)
	log.Printf("   Success Rate: %.1f%% (%d requests)",
		float64(result.SuccessRequests)/float64(result.TotalRequests)*100, result.SuccessRequests)
	log.Printf("   Error Rate: %.1f%% (%d requests)", result.ErrorRate, result.ErrorRequests)
	log.Printf("   Throughput: %.1f req/sec", result.RequestsPerSec)
	log.Printf("   Latency - Avg: %v, P95: %v, P99: %v",
		result.AvgLatency, result.P95Latency, result.P99Latency)
	log.Printf("Cache Performance:")
	log.Printf("   Hit Ratio: %.1f%% (%d hits, %d misses)",
		result.CacheStats.HitRatio, result.CacheStats.Hits, result.CacheStats.Misses)
	log.Printf("   Evictions: %d", result.CacheStats.Evictions)
	log.Printf("   Utilization: %.1f%% (%d/%d items)",
		result.CacheStats.Utilization, result.CacheStats.Size, result.CacheStats.Capacity)
	if result.MemoryUsage != nil {
		log.Printf("Resource Usage:")
		if mem, ok := result.MemoryUsage["memory_mb"].(string); ok {
			log.Printf("   Memory: %s MB", mem)
		}
		if goroutines, ok := result.MemoryUsage["goroutines"].(int); ok {
			log.Printf("   Goroutines: %d", goroutines)
		}
	}
	log.Println()
}

func (lt *LoadTester) RunAllScenarios() ([]*LoadTestResult, error) {
	scenarios := lt.GetTestScenarios()
	return lt.runScenarios(scenarios)
}

func (lt *LoadTester) RunScenariosForPolicy(policyName string) ([]*LoadTestResult, error) {
	scenarios := lt.GetScenariosForPolicy(policyName)
	log.Printf("🎯 Running %d scenarios optimized for %s policy", len(scenarios), policyName)
	return lt.runScenarios(scenarios)
}

func (lt *LoadTester) runScenarios(scenarios []LoadTestScenario) ([]*LoadTestResult, error) {
	results := make([]*LoadTestResult, 0, len(scenarios))

	log.Printf("Starting comprehensive eviction policy load testing")
	log.Printf("Will run %d scenarios", len(scenarios))
	log.Println()

	for i, scenario := range scenarios {
		log.Printf("Running scenario %d/%d", i+1, len(scenarios))

		result, err := lt.RunScenario(scenario)
		if err != nil {
			log.Printf("- Scenario '%s' failed: %v", scenario.Name, err)
			continue
		}

		results = append(results, result)

		if i < len(scenarios)-1 {
			log.Printf("Cooling down for 10 seconds...")
			time.Sleep(10 * time.Second)
		}
	}

	// Print comprehensive summary
	lt.printSummaryReport(results)

	return results, nil
}

func (lt *LoadTester) printSummaryReport(results []*LoadTestResult) {
	if len(results) == 0 {
		log.Printf(" - No results to summarize")
		return
	}

	log.Printf("LOAD TEST SUMMARY")
	log.Printf("=====================================")

	var totalRequests int64
	var totalSuccess int64
	var totalErrors int64
	var avgThroughput float64
	var avgHitRatio float64
	var totalEvictions int64

	for _, result := range results {
		totalRequests += result.TotalRequests
		totalSuccess += result.SuccessRequests
		totalErrors += result.ErrorRequests
		avgThroughput += result.RequestsPerSec
		avgHitRatio += result.CacheStats.HitRatio
		totalEvictions += result.CacheStats.Evictions
	}

	avgThroughput /= float64(len(results))
	avgHitRatio /= float64(len(results))

	log.Printf("Summary:")
	log.Printf("   Total Requests Processed: %d", totalRequests)
	log.Printf("   Overall Success Rate: %.1f%%", float64(totalSuccess)/float64(totalRequests)*100)
	log.Printf("   Average Throughput: %.1f req/sec", avgThroughput)
	log.Printf("   Average Cache Hit Ratio: %.1f%%", avgHitRatio)
	log.Printf("   Total Evictions: %d", totalEvictions)
	log.Println()

	log.Printf("Scenario Breakdown:")
	for _, result := range results {
		log.Printf("   %s:", result.Scenario)
		log.Printf("     Throughput: %.1f req/sec | Hit Ratio: %.1f%% | Evictions: %d",
			result.RequestsPerSec, result.CacheStats.HitRatio, result.CacheStats.Evictions)
	}
	log.Println()

	// Performance assessment
	log.Printf("Cache Performance:")
	if avgHitRatio >= 70 {
		log.Printf("   Cache hit ratio > 70 (%.1f%% avg)", avgHitRatio)
	} else if avgHitRatio >= 50 {
		log.Printf("   + Cache hit ratio > 50 (%.1f%% avg)", avgHitRatio)
	} else {
		log.Printf("   - Cache hit ratio (%.1f%% avg) - not good...", avgHitRatio)
	}

	if avgThroughput >= 1000 {
		log.Printf("   High throughput (%.1f req/sec avg)", avgThroughput)
	} else if avgThroughput >= 500 {
		log.Printf("   Moderate throughput (%.1f req/sec avg)", avgThroughput)
	} else {
		log.Printf("   Low throughput (%.1f req/sec avg) - performance bottleneck detected", avgThroughput)
	}

	successRate := float64(totalSuccess) / float64(totalRequests) * 100
	if successRate >= 99 {
		log.Printf("   + Reliability > 99 (%.1f%% success rate)", successRate)
	} else if successRate >= 95 {
		log.Printf("   + Reliability > 95 (%.1f%% success rate)", successRate)
	} else {
		log.Printf("   - Rliability (%.1f%% success rate) - stability issues?", successRate)
	}

	log.Printf("=====================================")
}
