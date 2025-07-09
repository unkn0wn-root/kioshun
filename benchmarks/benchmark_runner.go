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
			name:        "Comparison - Set Operations",
			pattern:     "BenchmarkCacheComparison_Set",
			description: "Pure write performance comparison",
			benchtime:   "5s",
		},
		{
			name:        "Comparison - Get Operations",
			pattern:     "BenchmarkCacheComparison_Get",
			description: "Pure read performance comparison",
			benchtime:   "5s",
		},
		{
			name:        "Comparison - Mixed Operations",
			pattern:     "BenchmarkCacheComparison_Mixed",
			description: "Mixed read/write workload comparison",
			benchtime:   "5s",
		},
		{
			name:        "Comparison - High Contention",
			pattern:     "BenchmarkCacheComparison_HighContention",
			description: "High contention scenario comparison",
			benchtime:   "5s",
		},
		{
			name:        "Comparison - Read Heavy",
			pattern:     "BenchmarkCacheComparison_ReadHeavy",
			description: "Read-heavy workload comparison",
			benchtime:   "3s",
		},
		{
			name:        "Comparison - Write Heavy",
			pattern:     "BenchmarkCacheComparison_WriteHeavy",
			description: "Write-heavy workload comparison",
			benchtime:   "3s",
		},
		{
			name:        "Comparison - Close to Real World",
			pattern:     "BenchmarkCacheComparison_RealWorldWorkload",
			description: "Realistic workload patterns",
			benchtime:   "3s",
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
	fmt.Printf("🏁 All benchmarks completed in %v\n", totalDuration)
	fmt.Println()
	fmt.Println("=== Summary ===")
	fmt.Println("The benchmarks compare kioshun cache against:")
	fmt.Println("- Ristretto (by Dgraph)")
	fmt.Println("- BigCache (by Allegro)")
	fmt.Println("- FreeCache (by Coocood)")
	fmt.Println("- Go-cache (by PatrickMN)")
	fmt.Println()
	fmt.Println("Key performance areas tested:")
	fmt.Println("- Pure read/write performance")
	fmt.Println("- Mixed workload scenarios")
	fmt.Println("- High contention handling")
	fmt.Println("- Memory efficiency")
	fmt.Println("- Eviction policy performance")
	fmt.Println("- Sharding effectiveness")
	fmt.Println("- Scalability under load")
}
