package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	cache "github.com/unkn0wn-root/kioshun"
	"github.com/unkn0wn-root/kioshun/cluster"
)

type setReq struct {
	K     string `json:"k"`
	V     string `json:"v"`
	TTLms int64  `json:"ttl_ms"`
}

func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func splitCSV(s string) []string {
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

func main() {
	port := getenv("PORT", "8081")
	bind := getenv("CACHE_BIND", ":5011")
	pub := getenv("CACHE_PUBLIC", "node1:5011")
	seeds := splitCSV(getenv("CACHE_SEEDS", pub))
	auth := getenv("CACHE_AUTH", "")
	allowKill := strings.ToLower(getenv("ALLOW_KILL", "false")) == "true"
	killToken := getenv("KILL_TOKEN", "")

	// in-process local cache
	local := cache.NewWithDefaults[string, []byte]()

	// cluster node
	cfg := cluster.Default()
	cfg.BindAddr = bind
	cfg.PublicURL = pub
	cfg.Seeds = seeds
	cfg.Sec.AuthToken = auth
	cfg.ID = cluster.NodeID(cfg.PublicURL)
	cfg.PerConnWorkers = 128
	cfg.PerConnQueue = 256
	cfg.Sec.MaxInflightPerPeer = 512

	// optional via env
	if v := getenv("REPLICATION_FACTOR", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.ReplicationFactor = n
		}
	} else {
		cfg.ReplicationFactor = 3
	}

	if v := getenv("WRITE_CONCERN", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.WriteConcern = n
		}
	} else {
		cfg.WriteConcern = 2
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

	kc := cluster.StringKeyCodec[string]{}
	vc := cluster.BytesCodec{}
	node := cluster.NewNode[string, []byte](cfg, kc, local, vc)
	if err := node.Start(); err != nil {
		log.Fatalf("start node: %v", err)
	}
	defer node.Stop()

	mux := http.NewServeMux()

	// GET /get?k=...
	mux.HandleFunc("/get", func(w http.ResponseWriter, r *http.Request) {
		k := r.URL.Query().Get("k")
		if k == "" {
			http.Error(w, "missing k", http.StatusBadRequest)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		v, found, err := node.Get(ctx, k)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		if !found {
			w.Header().Set("X-Cache", "MISS")
			http.Error(w, "not found", http.StatusNotFound)
			log.Printf("[MISS] k=%s", k)
			return
		}

		src := "HIT_REMOTE"
		if local.Exists(k) {
			src = "HIT_LOCAL"
		}
		w.Header().Set("X-Cache", src)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(v)
		log.Printf("[%s] k=%s sz=%d", src, k, len(v))
	})

	// POST /set  {k,v,ttl_ms}
	mux.HandleFunc("/set", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}

		var in setReq
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		ttl := time.Duration(in.TTLms) * time.Millisecond
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := node.Set(ctx, in.K, []byte(in.V), ttl); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
		log.Printf("[SET] k=%s ttl_ms=%d vlen=%d", in.K, in.TTLms, len(in.V))
	})

	// GET /stats  → local shard stats
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		st := local.Stats()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st)
	})

	// GET /ready simple readiness check
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// POST /kill?token=...&after_ms=0 — for failure injection during tests
	mux.HandleFunc("/kill", func(w http.ResponseWriter, r *http.Request) {
		if !allowKill {
			http.Error(w, "kill disabled", http.StatusForbidden)
			return
		}

		if killToken != "" && r.URL.Query().Get("token") != killToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		afterMs, _ := strconv.Atoi(r.URL.Query().Get("after_ms"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("bye"))
		go func() {
			if afterMs > 0 {
				time.Sleep(time.Duration(afterMs) * time.Millisecond)
			}
			log.Printf("[KILL] exiting process on request")
			node.Stop()
			time.Sleep(50 * time.Millisecond)
			os.Exit(0)
		}()
	})

	log.Printf("mesh node up on :%s | node %s bind %s seeds=%v", port, cfg.PublicURL, cfg.BindAddr, cfg.Seeds)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}
