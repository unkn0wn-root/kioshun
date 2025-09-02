package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type Stats struct {
	Total      int64
	Gets       int64
	Updates    int64
	Creates    int64
	Errs       int64
	CacheHits  int64
	DBHits     int64
	LatencySum int64 // microseconds
}

type User struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

func main() {
	target := getenv("TARGET", "http://localhost:8080")
	dur := getenv("DURATION", "30s")
	conc, _ := strconv.Atoi(getenv("CONCURRENCY", "64"))
	usersN, _ := strconv.Atoi(getenv("USERS", "200"))
	d, _ := time.ParseDuration(dur)
	rand.Seed(time.Now().UnixNano())

	// Pre-warm: touch users list once
	_, _ = http.Get(target + "/users")

	var st Stats
	var mu sync.Mutex

	stop := time.Now().Add(d)
	wg := sync.WaitGroup{}
	client := &http.Client{Timeout: 15 * time.Second}

	for i := 0; i < conc; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for time.Now().Before(stop) {
				op := rand.Intn(100)
				if op < 80 { // GET
					id := fmt.Sprintf("u%03d", rand.Intn(usersN))
					begin := time.Now()
					resp, err := client.Get(target + "/users/" + id)
					lat := time.Since(begin)

					atomic.AddInt64(&st.Total, 1)
					atomic.AddInt64(&st.Gets, 1)
					atomic.AddInt64(&st.LatencySum, lat.Microseconds())

					if err != nil {
						atomic.AddInt64(&st.Errs, 1)
						continue
					}

					src := resp.Header.Get("X-Source")
					if src == "cache" {
						atomic.AddInt64(&st.CacheHits, 1)
						log.Printf("[LOADGEN] GET %s → LOCAL CACHE (%.2fms)", id, float64(lat.Microseconds())/1000)
					} else if src == "remote-cache" {
						atomic.AddInt64(&st.CacheHits, 1)
						log.Printf("[LOADGEN] GET %s → REMOTE CACHE (%.2fms)", id, float64(lat.Microseconds())/1000)
					} else if src == "owner-routed" {
						atomic.AddInt64(&st.DBHits, 1)
						log.Printf("[LOADGEN] GET %s → OWNER ROUTED/DB (%.2fms)", id, float64(lat.Microseconds())/1000)
					} else if src == "db-fallback" {
						atomic.AddInt64(&st.DBHits, 1)
						log.Printf("[LOADGEN] GET %s → DB FALLBACK (%.2fms)", id, float64(lat.Microseconds())/1000)
					} else {
						atomic.AddInt64(&st.DBHits, 1)
						log.Printf("[LOADGEN] GET %s → UNKNOWN SOURCE '%s' (%.2fms)", id, src, float64(lat.Microseconds())/1000)
					}
					_ = resp.Body.Close()
				} else if op < 95 { // UPDATE
					id := fmt.Sprintf("u%03d", rand.Intn(usersN))
					name := fmt.Sprintf("User %s upd %d", id, time.Now().UnixNano())
					body, _ := json.Marshal(map[string]string{"name": name, "email": fmt.Sprintf("%s@example.com", id)})
					begin := time.Now()
					req, _ := http.NewRequest(http.MethodPut, target+"/users/update/"+id, bytes.NewReader(body))
					req.Header.Set("Content-Type", "application/json")
					resp, err := client.Do(req)
					lat := time.Since(begin)

					atomic.AddInt64(&st.Total, 1)
					atomic.AddInt64(&st.Updates, 1)
					atomic.AddInt64(&st.LatencySum, lat.Microseconds())

					if err != nil {
						atomic.AddInt64(&st.Errs, 1)
						continue
					}
					if resp.StatusCode/100 != 2 {
						atomic.AddInt64(&st.Errs, 1)
						_ = resp.Body.Close()
						continue
					}
					_ = resp.Body.Close()
					time.Sleep(50 * time.Millisecond)
					r2, err := client.Get(target + "/users/" + id)
					if err == nil {
						var u User
						_ = json.NewDecoder(r2.Body).Decode(&u)
						_ = r2.Body.Close()
						if u.Name != name {
							mu.Lock()
							log.Printf("STALE read after update: got %q want %q", u.Name, name)
							mu.Unlock()
						}
					}
				} else { // CREATE
					id := strconv.FormatInt(time.Now().UnixNano(), 10)
					name := "New " + id
					body, _ := json.Marshal(map[string]string{"name": name, "email": id + "@example.com"})
					begin := time.Now()
					resp, err := http.Post(target+"/users/create", "application/json", bytes.NewReader(body))
					lat := time.Since(begin)

					atomic.AddInt64(&st.Total, 1)
					atomic.AddInt64(&st.Creates, 1)
					atomic.AddInt64(&st.LatencySum, lat.Microseconds())
					if err != nil {
						atomic.AddInt64(&st.Errs, 1)
						continue
					}
					if resp.StatusCode/100 != 2 {
						atomic.AddInt64(&st.Errs, 1)
					}
					_ = resp.Body.Close()
				}
			}
		}(i)
	}

	wg.Wait()

	avgMs := float64(atomic.LoadInt64(&st.LatencySum)) / 1e3 / float64(max(atomic.LoadInt64(&st.Total), 1))
	fmt.Println("=== Loadgen Summary ===")
	fmt.Printf("Target: %s\n", target)
	fmt.Printf("Total: %d | GETs: %d | UPDATEs: %d | CREATEs: %d | Errors: %d\n",
		st.Total, st.Gets, st.Updates, st.Creates, st.Errs)
	fmt.Printf("Cache hits: %d | DB hits: %d\n", st.CacheHits, st.DBHits)
	fmt.Printf("Avg latency: %.2f ms\n", avgMs)
}

func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func max(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
