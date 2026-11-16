package httpcache

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// serveUser sends method path through h with X-User-ID set to user.
func serveUser(h http.Handler, method, path, user string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("X-User-ID", user)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// slotOf returns the epoch slot of the resource key of GET path.
func slotOf(m *Middleware, path string) uint64 {
	r := httptest.NewRequest("GET", path, nil)
	rk, _ := m.resourceKey(bareRequest(r, r.URL))
	return m.slotFor(rk)
}

func TestEpoch_AcceptHeaderFreeEntriesUntagged(t *testing.T) {
	for _, g := range generators {
		t.Run(g.name, func(t *testing.T) {
			e := newEnv(t, DefaultConfig(), variantHandler(200), withKeyGenerator(g.gen))
			e.do("GET", "/p")
			e.do("HEAD", "/p")
			if n := e.mw.cache.Size(); n != 2 {
				t.Fatalf("cache holds %d entries, want the GET and the HEAD", n)
			}
			for _, key := range e.mw.cache.Keys() {
				if resp, _ := e.mw.cache.Get(key); resp.slot != 0 {
					t.Errorf("entry %s stored without headers is tagged with slot %d, want untagged", key, resp.slot-1)
				}
			}
		})
	}
}

func TestEpoch_AcceptDefaultGeneratorHasNoTable(t *testing.T) {
	e := newEnv(t, DefaultConfig(), variantHandler(200))
	for _, user := range []string{"alice", "bob"} {
		e.do("GET", "/p", "X-User-ID", user, "Accept", user)
		e.do("HEAD", "/p", "X-User-ID", user, "Accept", user)
	}
	if e.mw.epochs != nil {
		t.Errorf("epoch table has %d slots without SetKeyGenerator, want none", len(e.mw.epochs))
	}
	if n := e.mw.cache.Size(); n != 2 {
		t.Fatalf("cache holds %d entries, want the GET and the HEAD", n)
	}
	for _, key := range e.mw.cache.Keys() {
		if resp, _ := e.mw.cache.Get(key); resp.slot != 0 {
			t.Errorf("entry %s is tagged under DefaultKeyGenerator, want untagged", key)
		}
	}
	e.do("POST", "/p", "X-User-ID", "bob")
	if r := e.do("GET", "/p", "X-User-ID", "alice", "Accept", "alice"); r.body != "v2" {
		t.Errorf("GET after POST got %q (X-Cache=%s), want v2", r.body, r.hdr.Get("X-Cache"))
	}
}

func TestEpoch_AcceptTableSize(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cfg   Config
		slots int
	}{
		{"MaxSize 100", Config{MaxSize: 100}, 1024},
		{"MaxSize 5000", Config{MaxSize: 5000}, 8192},
		{"no size limit", Config{DisableCacheSizeLimit: true}, 1 << 16},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestMiddleware(t, tc.cfg)
			defer m.Close()
			m.SetKeyGenerator(KeyWithUserID("X-User-ID"))
			if len(m.epochs) != tc.slots {
				t.Errorf("epoch table has %d slots, want %d", len(m.epochs), tc.slots)
			}
			table := &m.epochs[0]
			m.SetKeyGenerator(KeyWithVaryHeaders([]string{"Accept"}))
			if &m.epochs[0] != table {
				t.Error("a second SetKeyGenerator replaced the epoch table")
			}
		})
	}
}

func TestEpoch_AcceptSlotCollisionCostsOneMiss(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxSize = 1024
	m := newTestMiddleware(t, cfg)
	defer m.Close()
	m.SetKeyGenerator(KeyWithUserID("X-User-ID"))
	h := m.Wrap(variantHandler(200))
	other := ""
	for i := 0; other == ""; i++ {
		if p := "/b" + strconv.Itoa(i); slotOf(m, p) == slotOf(m, "/a") {
			other = p
		}
	}
	users := []string{"alice", "bob"}
	for _, path := range []string{"/a", other} {
		for _, user := range users {
			serveUser(h, "GET", path, user)
		}
	}
	serveUser(h, "POST", "/a", "bob")
	for _, path := range []string{"/a", other} {
		for _, user := range users {
			if rec := serveUser(h, "GET", path, user); rec.Body.String() != "v2" || rec.Header().Get("X-Cache") != "MISS" {
				t.Errorf("%s's first GET %s after POST /a got %q (X-Cache=%s), want v2 from the origin", user, path, rec.Body.String(), rec.Header().Get("X-Cache"))
			}
			if rec := serveUser(h, "GET", path, user); rec.Body.String() != "v2" || rec.Header().Get("X-Cache") != "HIT" {
				t.Errorf("%s's second GET %s after POST /a got %q (X-Cache=%s), want a v2 hit", user, path, rec.Body.String(), rec.Header().Get("X-Cache"))
			}
		}
	}
}

// Slot 0 is stored as 1, so a resource key in slot 0 is still tagged.
func TestEpoch_RejectStaleVariantInSlotZero(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxSize = 1024
	m := newTestMiddleware(t, cfg)
	defer m.Close()
	m.SetKeyGenerator(KeyWithUserID("X-User-ID"))
	h := m.Wrap(variantHandler(200))
	path := ""
	for i := 0; path == ""; i++ {
		if p := "/p" + strconv.Itoa(i); slotOf(m, p) == 0 {
			path = p
		}
	}
	serveUser(h, "GET", path, "alice")
	serveUser(h, "GET", path, "bob")
	serveUser(h, "POST", path, "bob")
	if rec := serveUser(h, "GET", path, "alice"); rec.Body.String() != "v2" {
		t.Errorf("alice got %q (X-Cache=%s) for %s in slot 0 after bob's POST, want v2", rec.Body.String(), rec.Header().Get("X-Cache"), path)
	}
}

// A write must remove the untagged entry and stop serving the tagged variants.
func TestEpoch_RejectStaleMixedUntaggedAndTagged(t *testing.T) {
	for _, g := range generators {
		t.Run(g.name, func(t *testing.T) {
			h := variantHeader(g.name)
			e := newEnv(t, DefaultConfig(), variantHandler(200), withKeyGenerator(g.gen))
			for _, method := range []string{"GET", "HEAD"} {
				e.do(method, "/p")
				e.do(method, "/p", h, "alice")
			}
			e.do("POST", "/p", h, "bob")
			if r := e.do("GET", "/p"); r.body != "v2" {
				t.Errorf("GET without the header got %q (X-Cache=%s), want v2", r.body, r.hdr.Get("X-Cache"))
			}
			if r := e.do("GET", "/p", h, "alice"); r.body != "v2" {
				t.Errorf("alice's GET got %q (X-Cache=%s), want v2", r.body, r.hdr.Get("X-Cache"))
			}
			if r := e.do("HEAD", "/p"); r.hdr.Get("X-Cache") != "MISS" {
				t.Errorf("HEAD without the header got X-Cache=%s, want MISS", r.hdr.Get("X-Cache"))
			}
			if r := e.do("HEAD", "/p", h, "alice"); r.hdr.Get("X-Cache") != "MISS" {
				t.Errorf("alice's HEAD got X-Cache=%s, want MISS", r.hdr.Get("X-Cache"))
			}
		})
	}
}

// An entry stored before the epoch wraps to 0 must not be served after it.
// The 32-bit rows cover the carry into the high half.
func TestEpoch_RejectStaleAcrossWraparound(t *testing.T) {
	for _, start := range []uint64{math.MaxUint32 - 1, math.MaxUint32, math.MaxUint64 - 1, math.MaxUint64} {
		t.Run(strconv.FormatUint(start, 10), func(t *testing.T) {
			m := newTestMiddleware(t, DefaultConfig())
			defer m.Close()
			m.SetKeyGenerator(KeyWithUserID("X-User-ID"))
			h := m.Wrap(variantHandler(200))
			m.epochs[slotOf(m, "/p")].Store(start)
			for post := 1; post <= 4; post++ {
				serveUser(h, "GET", "/p", "alice")
				if rec := serveUser(h, "GET", "/p", "alice"); rec.Header().Get("X-Cache") != "HIT" {
					t.Fatalf("round %d: second GET got X-Cache=%s, want HIT", post, rec.Header().Get("X-Cache"))
				}
				serveUser(h, "POST", "/p", "bob")
				want := "v" + strconv.Itoa(post+1)
				if rec := serveUser(h, "GET", "/p", "alice"); rec.Body.String() != want {
					t.Fatalf("round %d: alice got %q (X-Cache=%s), want %s", post, rec.Body.String(), rec.Header().Get("X-Cache"), want)
				}
			}
		})
	}
}

// A jump of 2^32 keeps the low 32 bits, so the whole epoch must be compared.
func TestEpoch_RejectStaleAfterJumpOf2To32(t *testing.T) {
	for _, jump := range []uint64{1 << 32, 1 << 33, 3 << 32, 1<<64 - 1<<32} {
		t.Run(strconv.FormatUint(jump, 16), func(t *testing.T) {
			m := newTestMiddleware(t, DefaultConfig())
			defer m.Close()
			m.SetKeyGenerator(KeyWithUserID("X-User-ID"))
			h := m.Wrap(variantHandler(200))
			serveUser(h, "GET", "/p", "alice")
			if rec := serveUser(h, "GET", "/p", "alice"); rec.Header().Get("X-Cache") != "HIT" {
				t.Fatalf("second GET got X-Cache=%s, want HIT", rec.Header().Get("X-Cache"))
			}
			m.epochs[slotOf(m, "/p")].Add(jump)
			if rec := serveUser(h, "GET", "/p", "alice"); rec.Header().Get("X-Cache") != "MISS" {
				t.Errorf("after the epoch moved on by %#x alice got X-Cache=%s, want MISS", jump, rec.Header().Get("X-Cache"))
			}
		})
	}
}

func TestEpoch_RejectStaleAfterManyInvalidations(t *testing.T) {
	m := newTestMiddleware(t, DefaultConfig())
	defer m.Close()
	m.SetKeyGenerator(KeyWithUserID("X-User-ID"))
	h := m.Wrap(variantHandler(200))
	users := []string{"alice", "bob", "carol"}
	for post := 1; post <= 1000; post++ {
		for _, user := range users {
			serveUser(h, "GET", "/p", user)
		}
		serveUser(h, "POST", "/p", "writer")
		want := "v" + strconv.Itoa(post+1)
		for _, user := range users {
			if got := serveUser(h, "GET", "/p", user).Body.String(); got != want {
				t.Fatalf("after POST %d %s got %q, want %s", post, user, got, want)
			}
		}
	}
}

func TestEpoch_AcceptConcurrentStoreInvalidate(t *testing.T) {
	m := newTestMiddleware(t, DefaultConfig())
	defer m.Close()
	m.SetKeyGenerator(KeyWithUserID("X-User-ID"))
	h := m.Wrap(variantHandler(200))

	const users = 50
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				serveUser(h, "GET", "/p", strconv.Itoa((g+i)%users))
			}
		}()
	}
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				serveUser(h, "POST", "/p", "writer")
			}
		}()
	}
	wg.Wait()
	serveUser(h, "POST", "/p", "writer") // no GET is in flight
	for u := range users {
		if rec := serveUser(h, "GET", "/p", strconv.Itoa(u)); rec.Header().Get("X-Cache") != "MISS" {
			t.Errorf("user %d after the last POST got X-Cache=%s, want MISS", u, rec.Header().Get("X-Cache"))
		}
	}
}

func TestClear_AcceptInvalidationAfterClear(t *testing.T) {
	t.Run("variants", func(t *testing.T) {
		e := newEnv(t, DefaultConfig(), variantHandler(200), withKeyGenerator(KeyWithUserID("X-User-ID")))
		e.do("GET", "/p", "X-User-ID", "alice")
		e.mw.Clear()
		if r := e.do("GET", "/p", "X-User-ID", "alice"); r.hdr.Get("X-Cache") != "MISS" {
			t.Errorf("GET after Clear got X-Cache=%s, want MISS", r.hdr.Get("X-Cache"))
		}
		if r := e.do("GET", "/p", "X-User-ID", "alice"); r.hdr.Get("X-Cache") != "HIT" {
			t.Errorf("second GET after Clear got X-Cache=%s, want HIT", r.hdr.Get("X-Cache"))
		}
		e.do("POST", "/p", "X-User-ID", "alice")
		if r := e.do("GET", "/p", "X-User-ID", "alice"); r.body != "v2" {
			t.Errorf("GET after POST got %q (X-Cache=%s), want v2", r.body, r.hdr.Get("X-Cache"))
		}
	})
	t.Run("pattern", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.PathExtractor = PathExtractorFromKey
		e := newEnv(t, cfg, variantHandler(200), withKeyGenerator(KeyWithoutQuery()))
		e.do("GET", "/p")
		e.mw.Clear()
		e.do("GET", "/p")
		if r := e.do("GET", "/p"); r.hdr.Get("X-Cache") != "HIT" {
			t.Errorf("second GET after Clear got X-Cache=%s, want HIT", r.hdr.Get("X-Cache"))
		}
		if n := e.mw.Invalidate("/p"); n != 1 {
			t.Errorf("Invalidate(/p) after Clear removed %d entries, want 1", n)
		}
	})
}

// Clear racing stores must not leave entries the index no longer knows.
func TestPatternIndex_RejectClearRacingStores(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PathExtractor = PathExtractorFromKey
	m := newTestMiddleware(t, cfg)
	defer m.Close()
	m.SetKeyGenerator(KeyWithoutQuery())
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=600")
		w.Write([]byte("x"))
	}))

	for round := range 20 {
		stop := make(chan struct{})
		var clears, stores sync.WaitGroup
		clears.Add(1)
		go func() {
			defer clears.Done()
			for {
				select {
				case <-stop:
					return
				default:
					m.Clear()
				}
			}
		}()
		for g := range 4 {
			stores.Add(1)
			go func() {
				defer stores.Done()
				for i := range 500 {
					h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", fmt.Sprintf("/r%d/g%d/k%d", round, g, i), nil))
				}
			}()
		}
		stores.Wait()
		close(stop)
		clears.Wait()

		m.Invalidate("/*")
		if n := m.InvalidateByFunc(func(string) bool { return true }); n > 0 {
			t.Fatalf("round %d: %d cached entries survived Invalidate(\"/*\") after Clear raced stores", round, n)
		}
	}
}

// stampede sends dup concurrent misses for each of n paths. The handler holds
// them until all have arrived, so their stores race.
func stampede(t *testing.T, n, dup int) *Middleware {
	t.Helper()
	cfg := DefaultConfig()
	cfg.PathExtractor = PathExtractorFromKey
	cfg.DisableCacheSizeLimit = true
	m := newTestMiddleware(t, cfg)
	t.Cleanup(func() { m.Close() })
	m.SetKeyGenerator(KeyWithoutQuery())
	var barrier atomic.Pointer[sync.WaitGroup]
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := barrier.Load()
		b.Done()
		b.Wait()
		w.Header().Set("Cache-Control", "max-age=600")
		w.Write([]byte("x"))
	}))
	for i := range n {
		b := new(sync.WaitGroup)
		b.Add(dup)
		barrier.Store(b)
		var wg sync.WaitGroup
		for range dup {
			wg.Add(1)
			go func() {
				defer wg.Done()
				h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", fmt.Sprintf("/orphan/%d", i), nil))
			}()
		}
		wg.Wait()
	}
	return m
}

func TestPatternIndex_AcceptIndexMatchesCacheAfterRace(t *testing.T) {
	m := stampede(t, 200, 8)
	indexed := m.patternIdx.getMatchingKeys("/orphan/*")
	cached := m.cache.Keys()
	slices.Sort(indexed)
	slices.Sort(cached)
	if !slices.Equal(indexed, cached) {
		t.Errorf("index holds %d keys, cache holds %d, want the same keys", len(indexed), len(cached))
	}
}

// Racing stores of one key must leave the index and the cache agreeing.
func TestPatternIndex_RejectOrphanFromConcurrentSameKeyStores(t *testing.T) {
	m := stampede(t, 1000, 8)
	m.InvalidateByFunc(func(string) bool { return true })
	deadline := time.Now().Add(2 * time.Second)
	for len(m.patternIdx.getMatchingKeys("/*")) > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := len(m.patternIdx.getMatchingKeys("/*")); n > 0 {
		t.Errorf("index still holds %d keys after every entry was deleted", n)
	}
}
