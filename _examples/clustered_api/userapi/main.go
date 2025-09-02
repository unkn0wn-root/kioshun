package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	cache "github.com/unkn0wn-root/kioshun"
	"github.com/unkn0wn-root/kioshun/cluster"
)

type User struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// mockDB simulates a slow backing store with random latency 1..4s.
type mockDB struct {
	mu    sync.RWMutex
	users map[string]User
}

func newMockDB() *mockDB {
	return &mockDB{users: make(map[string]User)}
}

func delay() {
	time.Sleep(time.Duration(1+rand.Intn(4)) * time.Second)
}

func (db *mockDB) GetUser(id string) (User, bool) {
	delay()
	db.mu.RLock()
	defer db.mu.RUnlock()
	u, ok := db.users[id]
	return u, ok
}

func (db *mockDB) GetUsers() []User {
	delay()
	db.mu.RLock()
	defer db.mu.RUnlock()
	out := make([]User, 0, len(db.users))
	for _, u := range db.users {
		out = append(out, u)
	}
	return out
}

func (db *mockDB) CreateUser(u User) User {
	delay()
	db.mu.Lock()
	defer db.mu.Unlock()
	if u.ID == "" {
		u.ID = strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	db.users[u.ID] = u
	return u
}

func (db *mockDB) UpdateUser(id string, u User) (User, bool) {
	delay()
	db.mu.Lock()
	defer db.mu.Unlock()
	if _, ok := db.users[id]; !ok {
		return User{}, false
	}
	u.ID = id
	db.users[id] = u
	return u, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	rand.Seed(time.Now().UnixNano())

	// env
	port := getenv("PORT", "8081")
	bind := getenv("CACHE_BIND", ":5011")
	pub := getenv("CACHE_PUBLIC", "localhost:5011")
	seeds := splitCSV(getenv("CACHE_SEEDS", pub))
	auth := getenv("CACHE_AUTH", "")
	seedUsers, _ := strconv.Atoi(getenv("SEED_USERS", "100"))

	// mock DB
	db := newMockDB()
	for i := 0; i < seedUsers; i++ {
		id := fmt.Sprintf("u%03d", i)
		db.users[id] = User{ID: id, Name: fmt.Sprintf("User %03d", i), Email: fmt.Sprintf("user%03d@example.com", i)}
	}

	// cache cluster node
	local := cache.NewWithDefaults[string, []byte]()
	cfg := cluster.Default()
	cfg.BindAddr = bind
	cfg.PublicURL = pub
	cfg.Seeds = seeds
	cfg.ReplicationFactor = 3
	cfg.WriteConcern = 2
	cfg.Sec.AuthToken = auth
	cfg.ID = cluster.NodeID(cfg.PublicURL)

	cfg.LeaseTTL = 8 * time.Second
	cfg.Sec.ReadTimeout = 12 * time.Second
	cfg.Sec.WriteTimeout = 6 * time.Second
	cfg.Sec.MaxInflightPerPeer = 512
	cfg.Sec.LeaseLoadQPS = 100
	cfg.ReadPerTryTimeout = 6 * time.Second
	cfg.ReadMaxFanout = 3
	cfg.PerConnWorkers = 128
	cfg.PerConnQueue = 256

	// Node loader: supports user:<id> and users:all keys
	var dbLoads atomic.Int64
	kc := cluster.StringKeyCodec[string]{}
	vc := cluster.BytesCodec{}
	node := cluster.NewNode[string, []byte](cfg, kc, local, vc)
	node.Loader = func(ctx context.Context, k string) ([]byte, time.Duration, error) {
		// identify key type
		if strings.HasPrefix(k, "user:") {
			id := strings.TrimPrefix(k, "user:")
			u, ok := db.GetUser(id)
			if !ok {
				return nil, 0, fmt.Errorf("not found")
			}

			dbLoads.Add(1)
			b, _ := json.Marshal(u)
			return b, 2 * time.Minute, nil
		}

		if k == "users:all" {
			list := db.GetUsers()
			dbLoads.Add(1)
			b, _ := json.Marshal(list)
			return b, 1 * time.Minute, nil
		}
		return nil, 0, fmt.Errorf("no loader for key")
	}

	if err := node.Start(); err != nil {
		log.Fatalf("start node: %v", err)
	}
	defer node.Stop()
	dc := cluster.NewDistributedCache[string, []byte](node)

	mux := http.NewServeMux()

	mux.HandleFunc("/users/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/users/")
		if id == "" {
			http.NotFound(w, r)
			return
		}
		key := "user:" + id
		if v, ok := dc.Get(key); ok {
			log.Printf("[CACHE HIT] user:%s from local cache", id)
			w.Header().Set("X-Source", "cache")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(v)
			return
		}

		// Try direct Get first to check remote cache
		if v, found, err := node.Get(context.Background(), key); err == nil && found {
			log.Printf("[REMOTE CACHE HIT] user:%s from remote node", id)
			w.Header().Set("X-Source", "remote-cache")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(v)
			return
		}

		// miss -> coordinated load on primary (this will hit DB)
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		b, err := node.GetOrLoad(ctx, key, func(context.Context) ([]byte, time.Duration, error) {
			// Local-primary path: load from mock DB
			id := strings.TrimPrefix(key, "user:")
			log.Printf("[DB HIT] Loading user:%s from database", id)
			u, ok := db.GetUser(id)
			if !ok {
				return nil, 0, fmt.Errorf("not found")
			}

			bs, _ := json.Marshal(u)
			return bs, 2 * time.Minute, nil
		})
		if err != nil {
			// Only 404 when the loader says not found; fallback to DB for other errors
			if strings.Contains(strings.ToLower(err.Error()), "not found") {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			// Fallback to direct DB hit on cluster errors
			log.Printf("[DB FALLBACK] Cluster error for user:%s, hitting DB directly: %v", id, err)
			u, ok := db.GetUser(id)
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}

			b, _ := json.Marshal(u)
			w.Header().Set("X-Source", "db-fallback")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(b)
			return
		}
		// Owner-routed path (may have just loaded on primary, or fetched from owner)
		log.Printf("[OWNER ROUTED] user:%s from cluster owner", id)
		w.Header().Set("X-Source", "owner-routed")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
	})

	// GET /users
	mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		key := "users:all"
		if v, ok := dc.Get(key); ok {
			log.Printf("[CACHE HIT] users:all from local cache")
			w.Header().Set("X-Source", "cache")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(v)
			return
		}

		// Try direct Get first to check remote cache
		if v, found, err := node.Get(context.Background(), key); err == nil && found {
			log.Printf("[REMOTE CACHE HIT] users:all from remote node")
			w.Header().Set("X-Source", "remote-cache")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(v)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		b, err := node.GetOrLoad(ctx, key, func(context.Context) ([]byte, time.Duration, error) {
			// Local-primary path: load from mock DB
			log.Printf("[DB HIT] Loading users:all from database")
			list := db.GetUsers()
			bs, _ := json.Marshal(list)
			return bs, 1 * time.Minute, nil
		})
		if err != nil {
			// Fallback to direct DB hit on cluster errors
			log.Printf("[DB FALLBACK] Cluster error for users:all, hitting DB directly: %v", err)
			list := db.GetUsers()
			b, _ := json.Marshal(list)
			w.Header().Set("X-Source", "db-fallback")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(b)
			return
		}

		log.Printf("[OWNER ROUTED] users:all from cluster owner")
		w.Header().Set("X-Source", "owner-routed")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
	})

	// POST /users  {name,email}
	mux.HandleFunc("/users/create", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}

		var in struct{ Name, Email string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		u := db.CreateUser(User{Name: in.Name, Email: in.Email})
		// update caches
		_ = dc.Set("user:"+u.ID, mustJSON(u), 2*time.Minute)
		_ = dc.Delete("users:all")
		writeJSON(w, http.StatusCreated, u)
	})

	// PUT /users/{id}
	mux.HandleFunc("/users/update/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.NotFound(w, r)
			return
		}

		id := strings.TrimPrefix(r.URL.Path, "/users/update/")
		if id == "" {
			http.NotFound(w, r)
			return
		}

		var in struct{ Name, Email string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		u, ok := db.UpdateUser(id, User{Name: in.Name, Email: in.Email})
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		// update caches
		_ = dc.Set("user:"+u.ID, mustJSON(u), 2*time.Minute)
		_ = dc.Delete("users:all")
		writeJSON(w, http.StatusOK, u)
	})

	srv := &http.Server{Addr: ":" + port, Handler: mux}

	log.Printf("userapi up on :%s | node %s bind %s seeds=%v", port, cfg.PublicURL, cfg.BindAddr, cfg.Seeds)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

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
