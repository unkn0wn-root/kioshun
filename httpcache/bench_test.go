package httpcache

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
)

var benchBody = []byte("ok")

// discardWriter keeps the response header and drops the body.
type discardWriter struct{ h http.Header }

func (w *discardWriter) Header() http.Header         { return w.h }
func (w *discardWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *discardWriter) WriteHeader(int)             {}

// newBenchMiddleware serves GET and HEAD with Cache-Control cc and other
// methods with 204. A nil gen keeps the default key generator.
func newBenchMiddleware(b *testing.B, cfg Config, gen KeyGenerator, cc string) (*Middleware, http.Handler) {
	b.Helper()
	m, err := New(cfg)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { m.Close() })
	if gen != nil {
		m.SetKeyGenerator(gen)
	}
	return m, m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if cc != "" {
			w.Header().Set("Cache-Control", cc)
		}
		w.Write(benchBody)
	}))
}

func benchRequest(method, target, user string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.Header.Set("X-User-ID", user)
	return r
}

// liveHeap returns the heap in use after a full collection.
func liveHeap() int64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return int64(ms.HeapAlloc)
}

// BenchmarkWrap_Hit measures a hit, served from one goroutine and from many.
func BenchmarkWrap_Hit(b *testing.B) {
	for _, g := range []struct {
		name string
		gen  KeyGenerator
	}{
		{"Default", nil},
		{"KeyWithUserID", KeyWithUserID("X-User-ID")},
	} {
		b.Run(g.name, func(b *testing.B) {
			_, h := newBenchMiddleware(b, DefaultConfig(), g.gen, "max-age=60")
			w := &discardWriter{h: http.Header{}}
			h.ServeHTTP(w, benchRequest("GET", "/items/1", "u1"))
			clear(w.h)
			h.ServeHTTP(w, benchRequest("GET", "/items/1", "u1"))
			if w.h.Get("X-Cache") != "HIT" {
				b.Fatalf("X-Cache = %q, want HIT", w.h.Get("X-Cache"))
			}
			b.Run("serial", func(b *testing.B) {
				r, w := benchRequest("GET", "/items/1", "u1"), &discardWriter{h: http.Header{}}
				for b.Loop() {
					clear(w.h)
					h.ServeHTTP(w, r)
				}
			})
			b.Run("parallel", func(b *testing.B) {
				b.RunParallel(func(pb *testing.PB) {
					r, w := benchRequest("GET", "/items/1", "u1"), &discardWriter{h: http.Header{}}
					for pb.Next() {
						clear(w.h)
						h.ServeHTTP(w, r)
					}
				})
			})
		})
	}
}

// BenchmarkWrap_MissStore measures a miss whose response is stored under a new
// key. Run it with -benchtime 100000x so the cache never fills.
func BenchmarkWrap_MissStore(b *testing.B) {
	cfg := DefaultConfig()
	cfg.MaxSize = 1 << 18
	for _, g := range []struct {
		name string
		gen  KeyGenerator
	}{
		{"Default", nil},
		{"KeyWithoutQuery", KeyWithoutQuery()},
		{"KeyWithUserID", KeyWithUserID("X-User-ID")},
	} {
		b.Run(g.name, func(b *testing.B) {
			_, h := newBenchMiddleware(b, cfg, g.gen, "max-age=60")
			r, w := benchRequest("GET", "/", "u1"), &discardWriter{h: http.Header{}}
			i := 0
			for b.Loop() {
				i++
				r.URL.Path = "/items/" + strconv.Itoa(i)
				clear(w.h)
				h.ServeHTTP(w, r)
			}
		})
	}
	b.Run("KeyWithUserID-parallel", func(b *testing.B) {
		_, h := newBenchMiddleware(b, cfg, KeyWithUserID("X-User-ID"), "max-age=60")
		var n atomic.Int64
		b.RunParallel(func(pb *testing.PB) {
			r, w := benchRequest("GET", "/", "u1"), &discardWriter{h: http.Header{}}
			for pb.Next() {
				r.URL.Path = "/items/" + strconv.FormatInt(n.Add(1), 10)
				clear(w.h)
				h.ServeHTTP(w, r)
			}
		})
	})
}

// BenchmarkWrap_EvictionChurn sends unique keys through a small cache, so every
// store evicts or is rejected. heap-B is the live heap after the run.
func BenchmarkWrap_EvictionChurn(b *testing.B) {
	for _, g := range []struct {
		name string
		gen  KeyGenerator
	}{
		{"Default", nil},
		{"KeyWithUserID", KeyWithUserID("X-User-ID")},
	} {
		b.Run(g.name, func(b *testing.B) {
			cfg := DefaultConfig()
			cfg.MaxSize = 1024
			m, h := newBenchMiddleware(b, cfg, g.gen, "max-age=60")
			r, w := benchRequest("GET", "/", "u1"), &discardWriter{h: http.Header{}}
			before := liveHeap()
			i := 0
			for b.Loop() {
				i++
				r.URL.Path = "/items/" + strconv.Itoa(i)
				clear(w.h)
				h.ServeHTTP(w, r)
			}
			if err := m.cache.Sync(); err != nil {
				b.Fatal(err)
			}
			b.ReportMetric(float64(liveHeap()-before), "heap-B")
		})
	}
}

// BenchmarkWrap_MissNoStore measures a miss whose response is not stored.
func BenchmarkWrap_MissNoStore(b *testing.B) {
	for _, g := range []struct {
		name string
		gen  KeyGenerator
	}{
		{"Default", nil},
		{"KeyWithoutQuery", KeyWithoutQuery()},
		{"KeyWithUserID", KeyWithUserID("X-User-ID")},
	} {
		b.Run(g.name, func(b *testing.B) {
			_, h := newBenchMiddleware(b, DefaultConfig(), g.gen, "")
			r, w := benchRequest("GET", "/items/1", "u1"), &discardWriter{h: http.Header{}}
			for b.Loop() {
				clear(w.h)
				h.ServeHTTP(w, r)
			}
		})
	}
}

// BenchmarkWrap_Unsafe measures a successful unsafe request. The variant rows
// time only the invalidating requests.
func BenchmarkWrap_Unsafe(b *testing.B) {
	b.Run("Default", func(b *testing.B) {
		_, h := newBenchMiddleware(b, DefaultConfig(), nil, "max-age=60")
		r, w := benchRequest("POST", "/items/1", "u1"), &discardWriter{h: http.Header{}}
		for b.Loop() {
			clear(w.h)
			h.ServeHTTP(w, r)
		}
	})
	for _, variants := range []int{100, 10000} {
		b.Run("KeyWithUserID-"+strconv.Itoa(variants)+"variants", func(b *testing.B) {
			_, h := newBenchMiddleware(b, DefaultConfig(), KeyWithUserID("X-User-ID"), "max-age=60")
			w := &discardWriter{h: http.Header{}}
			for i := range variants {
				clear(w.h)
				h.ServeHTTP(w, benchRequest("GET", "/items/1", strconv.Itoa(i)))
			}
			post := benchRequest("POST", "/items/1", "0")
			for b.Loop() {
				clear(w.h)
				h.ServeHTTP(w, post)
			}
		})
	}
}

// BenchmarkWrap_MemoryPerVariant reports the live heap per stored variant, 10
// per resource. Run it with a fixed -benchtime such as 3x.
func BenchmarkWrap_MemoryPerVariant(b *testing.B) {
	const resources, variants = 1000, 10
	var total, runs int64
	for b.Loop() {
		cfg := DefaultConfig()
		cfg.MaxSize = 1 << 16
		m, h := newBenchMiddleware(b, cfg, KeyWithUserID("X-User-ID"), "max-age=60")
		r, w := benchRequest("GET", "/", ""), &discardWriter{h: http.Header{}}
		before := liveHeap()
		for i := range resources {
			r.URL.Path = "/items/" + strconv.Itoa(i)
			for u := range variants {
				r.Header.Set("X-User-ID", strconv.Itoa(u))
				clear(w.h)
				h.ServeHTTP(w, r)
			}
		}
		if err := m.cache.Sync(); err != nil {
			b.Fatal(err)
		}
		if n := m.cache.Size(); n != resources*variants {
			b.Fatalf("cache holds %d entries, want %d", n, resources*variants)
		}
		total += liveHeap() - before
		runs++
		m.Close()
	}
	b.ReportMetric(float64(total)/float64(runs*resources*variants), "B/variant")
}
