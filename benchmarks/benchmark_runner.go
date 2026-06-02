package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

func main() {
	fmt.Println("=== KIOSHUN Cache Benchmark Suite ===")
	fmt.Println("Running benchmarks against popular Go caches")
	fmt.Println()

	benchmarks := []struct {
		name        string
		pattern     string
		description string
		benchtime   string
	}{
		{
			name:        "Comparison - Set Async",
			pattern:     "BenchmarkCacheComparison_Set_Async",
			description: "High-throughput writes; async caches flush outside timed work",
			benchtime:   "5s",
		},
		{
			name:        "Comparison - Set Strict",
			pattern:     "BenchmarkCacheComparison_Set_Strict",
			description: "Committed writes; async caches wait per mutation",
			benchtime:   "2s",
		},
		{
			name:        "Comparison - Get With TTL",
			pattern:     "BenchmarkCacheComparison_Get_TTL",
			description: "Pre-generated-key lookup comparison with one-hour expiration",
			benchtime:   "5s",
		},
		{
			name:        "Comparison - Get Without TTL",
			pattern:     "BenchmarkCacheComparison_Get_NoTTL",
			description: "Pre-generated-key lookup comparison without effective expiration",
			benchtime:   "5s",
		},
		{
			name:        "Comparison - Mixed Async",
			pattern:     "BenchmarkCacheComparison_Mixed_Async",
			description: "70/30 read/write workload; async caches flush outside timed work",
			benchtime:   "5s",
		},
		{
			name:        "Comparison - Mixed Strict",
			pattern:     "BenchmarkCacheComparison_Mixed_Strict",
			description: "70/30 read/write workload with committed writes",
			benchtime:   "2s",
		},
		{
			name:        "Comparison - Read Heavy Async",
			pattern:     "BenchmarkCacheComparison_ReadHeavy_Async",
			description: "90/10 read/write workload",
			benchtime:   "3s",
		},
		{
			name:        "Comparison - Write Heavy Async",
			pattern:     "BenchmarkCacheComparison_WriteHeavy_Async",
			description: "10/90 read/write workload",
			benchtime:   "3s",
		},
		{
			name:        "Comparison - High Contention Async",
			pattern:     "BenchmarkCacheComparison_HighContention_Async",
			description: "Hot-key mixed workload",
			benchtime:   "3s",
		},
		{
			name:        "Comparison - Real World Async",
			pattern:     "BenchmarkCacheComparison_RealWorld_Async",
			description: "60/35/5 read/write/delete workload; async caches flush outside timed work",
			benchtime:   "3s",
		},
		{
			name:        "Comparison - Real World Strict",
			pattern:     "BenchmarkCacheComparison_RealWorld_Strict",
			description: "60/35/5 read/write/delete workload with committed writes",
			benchtime:   "2s",
		},
		{
			name:        "Comparison - Large Values Async",
			pattern:     "BenchmarkCacheComparison_LargeValues_Async",
			description: "1-64KB value workload; async caches flush outside timed work",
			benchtime:   "3s",
		},
		{
			name:        "Comparison - Large Values Strict",
			pattern:     "BenchmarkCacheComparison_LargeValues_Strict",
			description: "1-64KB value workload with committed writes",
			benchtime:   "2s",
		},
		{
			name:        "Heavy Load Tests",
			pattern:     "BenchmarkCacheHeavyLoad",
			description: "Extreme load scenarios for kioshun",
			benchtime:   "3s",
		},
		{
			name:        "Contention Stress",
			pattern:     "BenchmarkCacheContentionStress",
			description: "High contention stress test for kioshun",
			benchtime:   "3s",
		},
		{
			name:        "Eviction Stress",
			pattern:     "BenchmarkCacheEvictionStress",
			description: "Heavy eviction testing for kioshun",
			benchtime:   "3s",
		},
		{
			name:        "Memory Pressure",
			pattern:     "BenchmarkCacheMemoryPressure",
			description: "Memory pressure testing for kioshun",
			benchtime:   "3s",
		},
		{
			name:        "Sharding Efficiency",
			pattern:     "BenchmarkCacheShardingEfficiency",
			description: "Sharding performance analysis for kioshun",
			benchtime:   "3s",
		},
	}

	totalStart := time.Now()

	for i, bench := range benchmarks {
		fmt.Printf("[%d/%d] %s\n", i+1, len(benchmarks), bench.name)
		fmt.Printf("Description: %s\n", bench.description)
		fmt.Printf("Running: go test -bench=%s -benchmem -benchtime=%s\n", bench.pattern, bench.benchtime)
		fmt.Println(strings.Repeat("-", 80))

		start := time.Now()

		cmd := exec.Command("go", "test", "-bench="+bench.pattern, "-benchmem", "-benchtime="+bench.benchtime, ".")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		err := cmd.Run()

		duration := time.Since(start)

		if err != nil {
			fmt.Printf(" - Benchmark failed: %v\n", err)
		} else {
			fmt.Printf(" + Benchmark completed in %v\n", duration)
		}

		fmt.Println()
	}

	totalDuration := time.Since(totalStart)
	fmt.Printf("All benchmarks completed in %v\n", totalDuration)
	fmt.Println()
	fmt.Println("=== Summary ===")
	fmt.Println("The benchmarks compare kioshun cache against:")
	fmt.Println("- Ristretto (by Dgraph)")
	fmt.Println("- BigCache (by Allegro)")
	fmt.Println("- FreeCache (by Coocood)")
	fmt.Println("- Go-cache (by PatrickMN)")
	fmt.Println()
	fmt.Println("Key performance areas tested:")
	fmt.Println("- Async and strict read/write performance")
	fmt.Println("- Mixed workload scenarios")
	fmt.Println("- High contention handling")
	fmt.Println("- Large value handling")
	fmt.Println("- Eviction policy performance")
	fmt.Println("- Sharding effectiveness")
	fmt.Println("- Scalability under load")
}
