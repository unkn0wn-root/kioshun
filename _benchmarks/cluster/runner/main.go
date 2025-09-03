package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type setReq struct {
	K     string `json:"k"`
	V     string `json:"v"`
	TTLms int64  `json:"ttl_ms"`
}

type stats struct {
	Total      int64
	Gets       int64
	Sets       int64
	Errs       int64
	HitLocal   int64
	HitRemote  int64
	Miss       int64
	WrongValue int64
	GetLatUs   []int64
	SetLatUs   []int64
}

func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func parseTargets(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func pick[T any](xs []T) T {
	return xs[rand.Intn(len(xs))]
}

func percentile(vals []int64, p float64) float64 {
	if len(vals) == 0 {
		return 0
	}

	cp := append([]int64(nil), vals...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	rank := p * float64(len(cp)-1)
	lo := int(rank)
	hi := lo + 1
	if hi >= len(cp) {
		return float64(cp[lo])
	}

	frac := rank - float64(lo)
	return float64(cp[lo])*(1-frac) + float64(cp[hi])*frac
}

// cache.Stats from node /stats
type CacheStats struct {
	Hits        int64   `json:"Hits"`
	Misses      int64   `json:"Misses"`
	Evictions   int64   `json:"Evictions"`
	Expirations int64   `json:"Expirations"`
	Size        int64   `json:"Size"`
	Capacity    int64   `json:"Capacity"`
	HitRatio    float64 `json:"HitRatio"`
	Shards      int     `json:"Shards"`
}

func main() {
	rand.Seed(time.Now().UnixNano())
	targets := parseTargets(getenv("TARGETS", "http://node1:8081,http://node2:8082,http://node3:8083"))
	d, _ := time.ParseDuration(getenv("DURATION", "60s"))
	conc, _ := strconv.Atoi(getenv("CONCURRENCY", "512"))
	keys, _ := strconv.Atoi(getenv("KEYS", "50000"))
	setRatio, _ := strconv.Atoi(getenv("SET_RATIO", "10")) // percent
	setTTLms, _ := strconv.ParseInt(getenv("SET_TTL_MS", "0"), 10, 64)
	logEvery, _ := strconv.Atoi(getenv("LOG_EVERY", "0"))

	// track expected value prefix to catch cross-key mismatches
	// value format: v:<key>:<seq>
	seqs := make([]uint64, keys)

	var st stats
	st.GetLatUs = make([]int64, 0, 2_000_000)
	st.SetLatUs = make([]int64, 0, 500_000)
	latMu := sync.Mutex{}

	deadline := time.Now().Add(d)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// trap SIGINT/SIGTERM so we always print summary.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigCh
		fmt.Println("[RUNNER] signal received, stopping...")
		cancel()
	}()

	wg := sync.WaitGroup{}
	client := &http.Client{Timeout: 8 * time.Second}

	// Periodic progress
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		lastTotal := int64(0)
		lastTime := time.Now()
		for {
			select {
			case now := <-ticker.C:
				t := atomic.LoadInt64(&st.Total)
				dt := now.Sub(lastTime).Seconds()
				if dt <= 0 {
					dt = 1
				}

				qps := float64(t-lastTotal) / dt
				hl := atomic.LoadInt64(&st.HitLocal)
				hr := atomic.LoadInt64(&st.HitRemote)
				ms := atomic.LoadInt64(&st.Miss)

				fmt.Printf("[PROGRESS] total=%d qps=%.0f hits(local=%d remote=%d) miss=%d\n", t, qps, hl, hr, ms)

				lastTotal = t
				lastTime = now
				if time.Now().After(deadline) {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// Periodic node stats (optional)
	if every := getenv("STATS_EVERY", ""); every != "" {
		if iv, err := time.ParseDuration(every); err == nil && iv > 0 {
			go func() {
				ticker := time.NewTicker(iv)
				defer ticker.Stop()
				for {
					select {
					case <-ticker.C:
						aggHits, aggMiss, aggEv, aggExp, aggSize, aggCap := int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)
						for _, t := range targets {
							cs, err := fetchStats(ctx, client, t)

							if err != nil {
								continue
							}

							aggHits += cs.Hits
							aggMiss += cs.Misses
							aggEv += cs.Evictions
							aggExp += cs.Expirations
							aggSize += cs.Size
							aggCap += cs.Capacity
						}
						getReqs := aggHits + aggMiss
						ratio := 0.0
						if getReqs > 0 {
							ratio = float64(aggHits) / float64(getReqs) * 100
						}

						fmt.Printf("[NODE STATS] hits=%d miss=%d hit_ratio=%.2f%% evictions=%d expirations=%d size=%d capacity=%d\n", aggHits, aggMiss, ratio, aggEv, aggExp, aggSize, aggCap)
					case <-ctx.Done():
						return
					}
				}
			}()
		}
	}

	// optional failure injection: kill one node
	if killMode := strings.ToLower(getenv("KILL_MODE", "none")); killMode != "none" {
		if ka := getenv("KILL_AFTER", ""); ka != "" {
			if after, err := time.ParseDuration(ka); err == nil && after > 0 {
				go func() {
					// Wait until either deadline or after
					t := time.NewTimer(after)
					defer t.Stop()
					select {
					case <-t.C:
					case <-ctx.Done():
						return
					}

					var target string
					switch killMode {
					case "random":
						target = pick(targets)
					case "target":
						target = getenv("KILL_TARGET", "")
						if target == "" {
							target = targets[0]
						}
					default:
						return
					}

					token := getenv("KILL_TOKEN", "")
					killURL := target + "/kill"
					if token != "" {
						killURL += "?token=" + token
					}

					req, _ := http.NewRequestWithContext(ctx, http.MethodPost, killURL, nil)
					if _, err := client.Do(req); err != nil {
						fmt.Printf("[KILL] request to %s failed: %v\n", target, err)
					} else {
						fmt.Printf("[KILL] requested shutdown of %s\n", target)
					}
				}()
			}
		}
	}

	for i := 0; i < conc; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			ops := 0
			for {
				if time.Now().After(deadline) {
					return
				}
				select {
				case <-ctx.Done():
					return
				default:
				}

				isSet := rand.Intn(100) < setRatio
				kidx := rand.Intn(keys)
				key := fmt.Sprintf("k%08d", kidx)
				base := pick(targets)
				if isSet {
					seq := atomic.AddUint64(&seqs[kidx], 1)
					val := fmt.Sprintf("v:%s:%d", key, seq)
					body, _ := json.Marshal(setReq{K: key, V: val, TTLms: setTTLms})

					begin := time.Now()
					req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/set", bytes.NewReader(body))
					req.Header.Set("Content-Type", "application/json")

					resp, err := client.Do(req)

					lat := time.Since(begin)

					atomic.AddInt64(&st.Total, 1)
					atomic.AddInt64(&st.Sets, 1)

					if err != nil {
						atomic.AddInt64(&st.Errs, 1)
					} else {
						_ = resp.Body.Close()
						latMu.Lock()
						st.SetLatUs = append(st.SetLatUs, lat.Microseconds())
						latMu.Unlock()
					}

					// occasionally validate from all nodes (read should retrieve expected key prefix)
					if seq%64 == 0 {
						for _, t := range targets {
							r2req, _ := http.NewRequestWithContext(ctx, http.MethodGet, t+"/get?k="+key, nil)
							r2, err2 := client.Do(r2req)
							if err2 == nil {
								buf := new(bytes.Buffer)
								_, _ = buf.ReadFrom(r2.Body)
								_ = r2.Body.Close()
								s := buf.String()
								if !strings.HasPrefix(s, "v:"+key+":") {
									atomic.AddInt64(&st.WrongValue, 1)
									log.Printf("[CONSISTENCY] wrong value for %s got=%q", key, s)
								}
							}
						}
					}
				} else { // GET
					begin := time.Now()
					req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/get?k="+key, nil)

					resp, err := client.Do(req)

					lat := time.Since(begin)

					atomic.AddInt64(&st.Total, 1)
					atomic.AddInt64(&st.Gets, 1)

					latMu.Lock()
					st.GetLatUs = append(st.GetLatUs, lat.Microseconds())
					latMu.Unlock()
					if err != nil {
						atomic.AddInt64(&st.Errs, 1)
					} else {
						src := resp.Header.Get("X-Cache")
						switch src {
						case "HIT_LOCAL":
							atomic.AddInt64(&st.HitLocal, 1)
						case "HIT_REMOTE":
							atomic.AddInt64(&st.HitRemote, 1)
						case "MISS":
							atomic.AddInt64(&st.Miss, 1)
						}
						// When hit, check prefix to avoid cross-key bugs
						if resp.StatusCode == 200 {
							buf := new(bytes.Buffer)
							_, _ = buf.ReadFrom(resp.Body)
							s := buf.String()
							if !strings.HasPrefix(s, "v:"+key+":") {
								atomic.AddInt64(&st.WrongValue, 1)
								log.Printf("[WRONG] key=%s got=%q src=%s", key, s, src)
							}
						}
						_ = resp.Body.Close()
					}
				}

				ops++
				if logEvery > 0 && ops%logEvery == 0 {
					hl := atomic.LoadInt64(&st.HitLocal)
					hr := atomic.LoadInt64(&st.HitRemote)
					ms := atomic.LoadInt64(&st.Miss)
					fmt.Printf("[WORKER %d] ops=%d hits(local=%d remote=%d) miss=%d\n", worker, ops, hl, hr, ms)
				}
			}
		}(i)
	}

	wg.Wait()

	// Summarize
	latMu.Lock()
	getP50 := percentile(st.GetLatUs, 0.50) / 1000
	getP95 := percentile(st.GetLatUs, 0.95) / 1000
	getP99 := percentile(st.GetLatUs, 0.99) / 1000
	getP999 := percentile(st.GetLatUs, 0.999) / 1000
	setP50 := percentile(st.SetLatUs, 0.50) / 1000
	setP95 := percentile(st.SetLatUs, 0.95) / 1000
	setP99 := percentile(st.SetLatUs, 0.99) / 1000
	setP999 := percentile(st.SetLatUs, 0.999) / 1000
	latMu.Unlock()

	total := atomic.LoadInt64(&st.Total)
	gets := atomic.LoadInt64(&st.Gets)
	sets := atomic.LoadInt64(&st.Sets)
	errs := atomic.LoadInt64(&st.Errs)
	hl := atomic.LoadInt64(&st.HitLocal)
	hr := atomic.LoadInt64(&st.HitRemote)
	ms := atomic.LoadInt64(&st.Miss)
	wrong := atomic.LoadInt64(&st.WrongValue)
	hitRatio := 0.0
	if gets > 0 {
		hitRatio = float64(hl+hr) / float64(gets)
	}

	fmt.Println("=== Mesh Bench Summary ===")
	fmt.Printf("Targets: %v\n", targets)
	fmt.Printf("Total: %d | GETs: %d | SETs: %d | Errors: %d\n", total, gets, sets, errs)
	fmt.Printf("Hits: local=%d remote=%d | Miss=%d | WrongValue=%d | HitRatio=%.2f%%\n", hl, hr, ms, wrong, hitRatio*100)
	fmt.Printf("GET p50=%.2fms p95=%.2fms p99=%.2fms p99.9=%.2fms | SET p50=%.2fms p95=%.2fms p99=%.2fms p99.9=%.2fms\n", getP50, getP95, getP99, getP999, setP50, setP95, setP99, setP999)

	// Final aggregated node stats
	aggHits, aggMiss, aggEv, aggExp, aggSize, aggCap := int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)
	for _, t := range targets {
		if cs, err := fetchStats(context.Background(), client, t); err == nil {
			aggHits += cs.Hits
			aggMiss += cs.Misses
			aggEv += cs.Evictions
			aggExp += cs.Expirations
			aggSize += cs.Size
			aggCap += cs.Capacity
		}
	}

	getReqs := aggHits + aggMiss
	ratio := 0.0
	if getReqs > 0 {
		ratio = float64(aggHits) / float64(getReqs) * 100
	}
	fmt.Printf("Node aggregate: hits=%d miss=%d hit_ratio=%.2f%% evictions=%d expirations=%d size=%d capacity=%d\n", aggHits, aggMiss, ratio, aggEv, aggExp, aggSize, aggCap)
}

func fetchStats(ctx context.Context, client *http.Client, base string) (CacheStats, error) {
	var out CacheStats
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/stats", nil)
	resp, err := client.Do(req)
	if err != nil {
		return out, err
	}

	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, err
	}
	return out, nil
}
