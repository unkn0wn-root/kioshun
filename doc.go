// Package kioshun provides a generic, sharded in-memory cache.
//
// # Background goroutines
//
// Each cache runs one write-worker goroutine per shard, plus one cleanup
// goroutine when CleanupInterval > 0. ShardCount therefore sets the background
// goroutine count; it defaults to min(NumCPU*4, 256). Applications that create
// many caches (for example through a Manager) should set Config.ShardCount
// explicitly to bound the total number of goroutines.
package kioshun
