package main

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	cache "github.com/unkn0wn-root/kioshun"
	"github.com/unkn0wn-root/kioshun/cluster"
)

type stats struct {
	Total      int64
	Gets       int64
	Sets       int64
	Errs       int64
	Hits       int64
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

func applyNodeEnv(cfg *cluster.Config) {
	if v := getenv("REPLICATION_FACTOR", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.ReplicationFactor = n
		}
	}

	if v := getenv("WRITE_CONCERN", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.WriteConcern = n
		}
	}

	if v := getenv("READ_MAX_FANOUT", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			cfg.ReadMaxFanout = n
		}
	}

	if v := getenv("READ_PER_TRY_MS", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.ReadPerTryTimeout = time.Duration(n) * time.Millisecond
		}
	}

	if v := getenv("READ_HEDGE_DELAY_MS", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.ReadHedgeDelay = time.Duration(n) * time.Millisecond
		}
	}

	if v := getenv("READ_HEDGE_INTERVAL_MS", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.ReadHedgeInterval = time.Duration(n) * time.Millisecond
		}
	}

	if v := getenv("WRITE_TIMEOUT_MS", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Sec.WriteTimeout = time.Duration(n) * time.Millisecond
		}
	}

	if v := getenv("READ_TIMEOUT_MS", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Sec.ReadTimeout = time.Duration(n) * time.Millisecond
		}
	}

	if v := getenv("SUSPICION_AFTER_MS", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.SuspicionAfter = time.Duration(n) * time.Millisecond
		}
	}

	if v := getenv("WEIGHT_UPDATE_MS", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.WeightUpdate = time.Duration(n) * time.Millisecond
		}
	}

	if v := getenv("GOSSIP_INTERVAL_MS", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.GossipInterval = time.Duration(n) * time.Millisecond
		}
	}
}

func main() {
	rand.Seed(time.Now().UnixNano())
	d, _ := time.ParseDuration(getenv("DURATION", "60s"))
	conc, _ := strconv.Atoi(getenv("CONCURRENCY", "512"))
	keys, _ := strconv.Atoi(getenv("KEYS", "50000"))
	setRatio, _ := strconv.Atoi(getenv("SET_RATIO", "10"))
	setTTLms, _ := strconv.ParseInt(getenv("SET_TTL_MS", "-1"), 10, 64)
	auth := getenv("CACHE_AUTH", "")

	// ports and addresses for the three nodes (within this process)
	bind1 := getenv("BIND1", ":7011")
	pub1 := getenv("PUB1", "127.0.0.1:7011")
	bind2 := getenv("BIND2", ":7012")
	pub2 := getenv("PUB2", "127.0.0.1:7012")
	bind3 := getenv("BIND3", ":7013")
	pub3 := getenv("PUB3", "127.0.0.1:7013")
	seeds := []string{pub1, pub2, pub3}

	// create three nodes
	mk := func(bind, pub string) *cluster.Node[string, []byte] {
		local := cache.NewWithDefaults[string, []byte]()
		cfg := cluster.Default()
		cfg.BindAddr = bind
		cfg.PublicURL = pub
		cfg.Seeds = seeds
		cfg.Sec.AuthToken = auth
		cfg.ID = cluster.NodeID(cfg.PublicURL)
		cfg.ReplicationFactor = 3
		cfg.WriteConcern = 2
		cfg.PerConnWorkers = 128
		cfg.PerConnQueue = 256
		cfg.Sec.MaxInflightPerPeer = 512
		applyNodeEnv(&cfg)

		n := cluster.NewNode[string, []byte](cfg, cluster.StringKeyCodec[string]{}, local, cluster.BytesCodec{})
		if err := n.Start(); err != nil {
			panic(err)
		}
		return n
	}

	n1 := mk(bind1, pub1)
	n2 := mk(bind2, pub2)
	n3 := mk(bind3, pub3)
	defer n1.Stop()
	defer n2.Stop()
	defer n3.Stop()

	// wait until ring is usable: best-effort small Set retry
	readyCtx, cancelReady := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelReady()

	for {
		if err := n1.Set(readyCtx, "__warmup__", []byte("ok"), time.Second); err == nil {
			break
		}

		select {
		case <-time.After(50 * time.Millisecond):
		case <-readyCtx.Done():
		}
		if readyCtx.Err() != nil {
			break
		}
	}

	// drive load via node1 client API
	driver := n1

	deadline := time.Now().Add(d)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("[RUNNER] signal received, stopping...")
		cancel()
	}()

	// optional failure injection: stop node3
	if ka := getenv("KILL_AFTER", ""); ka != "" {
		if after, err := time.ParseDuration(ka); err == nil && after > 0 {
			go func() {
				t := time.NewTimer(after)
				defer t.Stop()

				select {
				case <-t.C:
				case <-ctx.Done():
					return
				}

				fmt.Println("[KILL] stopping node3")
				n3.Stop()
			}()
		}
	}

	var st stats
	st.GetLatUs = make([]int64, 0, 2_000_000)
	st.SetLatUs = make([]int64, 0, 500_000)
	latMu := sync.Mutex{}

	// Progress
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

				fmt.Printf("[PROGRESS] total=%d qps=%.0f hits=%d miss=%d\n", t, qps, atomic.LoadInt64(&st.Hits), atomic.LoadInt64(&st.Miss))

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

	// Workload
	seqs := make([]uint64, keys)
	wg := sync.WaitGroup{}
	for i := 0; i < conc; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
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
				if isSet {
					seq := atomic.AddUint64(&seqs[kidx], 1)
					val := []byte(fmt.Sprintf("v:%s:%d", key, seq))
					ttl := time.Duration(0)
					if setTTLms < 0 {
						ttl = 0
					} else if setTTLms > 0 {
						ttl = time.Duration(setTTLms) * time.Millisecond
					}

					begin := time.Now()
					if err := driver.Set(ctx, key, val, ttl); err != nil {
						atomic.AddInt64(&st.Errs, 1)
					} else {
						latMu.Lock()
						st.SetLatUs = append(st.SetLatUs, time.Since(begin).Microseconds())
						latMu.Unlock()
						atomic.AddInt64(&st.Sets, 1)
						atomic.AddInt64(&st.Total, 1)
					}
				} else {
					begin := time.Now()
					_, ok, err := driver.Get(ctx, key)
					lat := time.Since(begin)

					atomic.AddInt64(&st.Total, 1)
					atomic.AddInt64(&st.Gets, 1)

					latMu.Lock()
					st.GetLatUs = append(st.GetLatUs, lat.Microseconds())
					latMu.Unlock()
					if err != nil {
						atomic.AddInt64(&st.Errs, 1)
						continue
					}

					if ok {
						atomic.AddInt64(&st.Hits, 1)
					} else {
						atomic.AddInt64(&st.Miss, 1)
					}
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

	hitRatio := 0.0
	if st.Gets > 0 {
		hitRatio = float64(st.Hits) / float64(st.Gets) * 100
	}
	fmt.Println("=== Kioshun Direct Bench Summary ===")
	fmt.Printf("Nodes: %s,%s,%s\n", pub1, pub2, pub3)
	fmt.Printf("Total: %d | GETs: %d | SETs: %d | Errors: %d\n", st.Total, st.Gets, st.Sets, st.Errs)
	fmt.Printf("Hits: %d | Miss: %d | HitRatio=%.2f%%\n", st.Hits, st.Miss, hitRatio)
	fmt.Printf("GET p50=%.2fms p95=%.2fms p99=%.2fms p99.9=%.2fms | SET p50=%.2fms p95=%.2fms p99=%.2fms p99.9=%.2fms\n", getP50, getP95, getP99, getP999, setP50, setP95, setP99, setP999)
}
