package httpcache

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// These tests check what RFC 9111 and RFC 9110 require of a shared cache, plus
// the stricter defaults (TestHTTPCache_*). Each runs a real HTTP server.

type env struct {
	t      *testing.T
	srv    *httptest.Server
	mw     *Middleware
	calls  atomic.Int64
	client *http.Client
}

type result struct {
	status int
	body   string
	hdr    http.Header
}

func newEnv(t *testing.T, cfg Config, h http.HandlerFunc, setup ...func(*Middleware)) *env {
	t.Helper()
	mw, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, f := range setup {
		f(mw)
	}
	e := &env{t: t, mw: mw}
	e.srv = httptest.NewServer(mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.calls.Add(1)
		h(w, r)
	})))
	e.client = &http.Client{
		// No automatic Accept-Encoding, so tests control every request header.
		Transport:     &http.Transport{DisableCompression: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	t.Cleanup(func() {
		e.srv.Close()
		if err := mw.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return e
}

// do sends one request. kv are header name/value pairs. "Host" sets r.Host.
func (e *env) do(method, path string, kv ...string) result {
	e.t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, nil)
	if err != nil {
		e.t.Fatalf("NewRequest: %v", err)
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i] == "Host" {
			req.Host = kv[i+1]
			continue
		}
		req.Header.Add(kv[i], kv[i+1])
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		e.t.Fatalf("read body: %v", err)
	}
	return result{status: resp.StatusCode, body: string(b), hdr: resp.Header}
}

func (e *env) origin() int64 { return e.calls.Load() }

// respond returns a handler that adds the header pairs in kv, writes status
// and a body that changes on every origin call ("body-1", "body-2", ...).
func respond(status int, kv ...string) http.HandlerFunc {
	var n atomic.Int64
	return func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i+1 < len(kv); i += 2 {
			w.Header().Add(kv[i], kv[i+1])
		}
		w.WriteHeader(status)
		fmt.Fprintf(w, "body-%d", n.Add(1))
	}
}

func wantOrigin(t *testing.T, e *env, want int64, why string) {
	t.Helper()
	if got := e.origin(); got != want {
		t.Errorf("origin calls = %d, want %d: %s", got, want, why)
	}
}

func httpDate(t time.Time) string { return t.UTC().Format(http.TimeFormat) }

// Response no-store (RFC 9111 5.2.2.5, RFC 9110 5.3).

func TestRFC9111_NoStore_AcceptMaxAge(t *testing.T) {
	e := newEnv(t, DefaultConfig(), respond(200, "Cache-Control", "max-age=60"))
	e.do("GET", "/r")
	r := e.do("GET", "/r")
	wantOrigin(t, e, 1, "max-age=60 response should be reused")
	if r.hdr.Get("X-Cache") != "HIT" {
		t.Errorf("X-Cache = %q, want HIT", r.hdr.Get("X-Cache"))
	}
}

func TestRFC9111_NoStore_RejectSecondFieldLine(t *testing.T) {
	e := newEnv(t, DefaultConfig(), respond(200, "Cache-Control", "max-age=60", "Cache-Control", "no-store"))
	e.do("GET", "/r")
	e.do("GET", "/r")
	wantOrigin(t, e, 2, "no-store on a second Cache-Control line must prevent storage")
}

func TestRFC9111_NoStore_RejectCustomPolicy(t *testing.T) {
	e := newEnv(t, DefaultConfig(), respond(200, "Cache-Control", "no-store"), func(m *Middleware) {
		m.SetCachePolicy(AlwaysCache(time.Minute))
	})
	e.do("GET", "/r")
	e.do("GET", "/r")
	wantOrigin(t, e, 2, "no-store must prevent storage under any Policy")
}

// private (RFC 9111 3, 5.2.2.7).

func TestRFC9111_Private_AcceptPublic(t *testing.T) {
	e := newEnv(t, DefaultConfig(), respond(200, "Cache-Control", "public, max-age=60"))
	e.do("GET", "/r")
	e.do("GET", "/r")
	wantOrigin(t, e, 1, "public, max-age=60 should be reused")
}

func TestRFC9111_Private_RejectSecondFieldLine(t *testing.T) {
	e := newEnv(t, DefaultConfig(), respond(200, "Cache-Control", "max-age=60", "Cache-Control", "private"))
	e.do("GET", "/me")
	e.do("GET", "/me")
	wantOrigin(t, e, 2, "private on a second Cache-Control line must prevent storage")
}

// no-cache (RFC 9111 4, 5.2.2.4).

func TestRFC9111_NoCache_AcceptWithoutDirective(t *testing.T) {
	e := newEnv(t, DefaultConfig(), respond(200, "Cache-Control", "max-age=60"))
	e.do("GET", "/r")
	e.do("GET", "/r")
	wantOrigin(t, e, 1, "max-age=60 without no-cache should be reused")
}

func TestRFC9111_NoCache_RejectSecondFieldLine(t *testing.T) {
	e := newEnv(t, DefaultConfig(), respond(200, "Cache-Control", "max-age=60", "Cache-Control", "no-cache"))
	e.do("GET", "/r")
	e.do("GET", "/r")
	wantOrigin(t, e, 2, "no-cache on a second Cache-Control line must not be reused without validation")
}

// Stale on arrival (RFC 9111 4.2.1, 4.2.4).

func TestRFC9111_MaxAge_AcceptPositive(t *testing.T) {
	e := newEnv(t, DefaultConfig(), respond(200, "Cache-Control", "max-age=60"))
	e.do("GET", "/r")
	e.do("GET", "/r")
	wantOrigin(t, e, 1, "max-age=60 should be reused")
}

func TestRFC9111_MaxAge_RejectZero(t *testing.T) {
	e := newEnv(t, DefaultConfig(), respond(200, "Cache-Control", "max-age=0"))
	e.do("GET", "/r")
	e.do("GET", "/r")
	wantOrigin(t, e, 2, "max-age=0 is stale on arrival")
}

// delta-seconds parsing (RFC 9111 1.2.2, 5.2).

func TestRFC9111_MaxAge_AcceptQuotedAndHuge(t *testing.T) {
	for _, cc := range []string{`max-age="60"`, "max-age=99999999999"} {
		t.Run(cc, func(t *testing.T) {
			e := newEnv(t, DefaultConfig(), respond(200, "Cache-Control", cc))
			e.do("GET", "/r")
			e.do("GET", "/r")
			wantOrigin(t, e, 1, "a quoted or overflowing max-age is a valid positive lifetime")
		})
	}
}

func TestRFC9111_MaxAge_RejectQuotedZero(t *testing.T) {
	e := newEnv(t, DefaultConfig(), respond(200, "Cache-Control", `max-age="0"`))
	e.do("GET", "/r")
	e.do("GET", "/r")
	wantOrigin(t, e, 2, `max-age="0" is stale on arrival`)
}

// max-age overrides Expires (RFC 9111 5.3).

func TestRFC9111_Expires_AcceptIgnoredUnderMaxAge(t *testing.T) {
	past := httpDate(time.Now().Add(-time.Hour))
	e := newEnv(t, DefaultConfig(), respond(200, "Cache-Control", "max-age=60", "Expires", past))
	e.do("GET", "/r")
	e.do("GET", "/r")
	wantOrigin(t, e, 1, "a past Expires is ignored when max-age is present")
}

func TestRFC9111_Expires_RejectIgnoredUnderMaxAgeZero(t *testing.T) {
	future := httpDate(time.Now().Add(time.Hour))
	e := newEnv(t, DefaultConfig(), respond(200, "Cache-Control", "max-age=0", "Expires", future))
	e.do("GET", "/r")
	e.do("GET", "/r")
	wantOrigin(t, e, 2, "a future Expires is ignored when max-age=0 is present")
}

// s-maxage overrides max-age and Expires for a shared cache (RFC 9111 4.2.1, 5.2.2.10).

func TestRFC9111_SMaxAge_AcceptOverridesMaxAgeZero(t *testing.T) {
	e := newEnv(t, DefaultConfig(), respond(200, "Cache-Control", "max-age=0, s-maxage=60"))
	e.do("GET", "/r")
	e.do("GET", "/r")
	wantOrigin(t, e, 1, "s-maxage=60 overrides max-age=0")
}

func TestRFC9111_SMaxAge_RejectZero(t *testing.T) {
	e := newEnv(t, DefaultConfig(), respond(200, "Cache-Control", "max-age=600, s-maxage=0"))
	e.do("GET", "/r")
	e.do("GET", "/r")
	wantOrigin(t, e, 2, "s-maxage=0 makes the response stale for a shared cache")
}

// must-revalidate (RFC 9111 5.2.2.2).

func TestRFC9111_MustRevalidate_AcceptFresh(t *testing.T) {
	e := newEnv(t, DefaultConfig(), respond(200, "Cache-Control", "must-revalidate, max-age=60"))
	e.do("GET", "/r")
	e.do("GET", "/r")
	wantOrigin(t, e, 1, "a fresh must-revalidate response may be reused")
}

func TestRFC9111_MustRevalidate_RejectStale(t *testing.T) {
	e := newEnv(t, DefaultConfig(), respond(200, "Cache-Control", "must-revalidate, max-age=0"))
	e.do("GET", "/r")
	e.do("GET", "/r")
	wantOrigin(t, e, 2, "a stale must-revalidate response must not be reused")
}

// Expires parsing (RFC 9111 5.3, RFC 9110 5.6.7).

func TestRFC9111_Expires_AcceptFutureANSIC(t *testing.T) {
	now := time.Now().UTC()
	e := newEnv(t, DefaultConfig(), respond(200,
		"Date", httpDate(now),
		"Expires", now.Add(60*time.Second).Format(time.ANSIC)))
	e.do("GET", "/r")
	e.do("GET", "/r")
	wantOrigin(t, e, 1, "an asctime Expires 60s after Date should be reused")
}

func TestRFC9111_Expires_RejectInvalid(t *testing.T) {
	// public would allow DefaultTTL, so only the invalid Expires prevents storage.
	e := newEnv(t, DefaultConfig(), respond(200, "Cache-Control", "public", "Expires", "0"))
	e.do("GET", "/r")
	e.do("GET", "/r")
	wantOrigin(t, e, 2, `an invalid Expires ("0") means already expired`)
}

func TestRFC9111_Expires_RejectPast(t *testing.T) {
	now := time.Now()
	e := newEnv(t, DefaultConfig(), respond(200,
		"Cache-Control", "public",
		"Date", httpDate(now),
		"Expires", httpDate(now.Add(-time.Hour))))
	e.do("GET", "/r")
	e.do("GET", "/r")
	wantOrigin(t, e, 2, "a past Expires means stale on arrival")
}

// current_age against freshness_lifetime (RFC 9111 4.2.3).

func TestRFC9111_Age_AcceptYoungerThanLifetime(t *testing.T) {
	e := newEnv(t, DefaultConfig(), respond(200,
		"Cache-Control", "max-age=60",
		"Date", httpDate(time.Now().Add(-10*time.Second))))
	e.do("GET", "/r")
	e.do("GET", "/r")
	wantOrigin(t, e, 1, "a 10s old response with max-age=60 is fresh")
}

func TestRFC9111_Age_RejectDateSkew(t *testing.T) {
	// The origin's Date is an hour old, so apparent_age exceeds the lifetime.
	then := time.Now().Add(-time.Hour)
	for name, kv := range map[string][]string{
		"max-age": {"Cache-Control", "max-age=60", "Date", httpDate(then)},
		"Expires": {"Date", httpDate(then), "Expires", httpDate(then.Add(30 * time.Second))},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, DefaultConfig(), respond(200, kv...))
			e.do("GET", "/r")
			e.do("GET", "/r")
			wantOrigin(t, e, 2, "current_age (1h) exceeds freshness_lifetime")
		})
	}
}

func TestRFC9111_Age_RejectAgeHeaderExceedsMaxAge(t *testing.T) {
	e := newEnv(t, DefaultConfig(), respond(200, "Cache-Control", "max-age=60", "Age", "120"))
	e.do("GET", "/r")
	e.do("GET", "/r")
	wantOrigin(t, e, 2, "Age: 120 exceeds max-age=60")
}

// Age is generated on reuse (RFC 9111 4, 5.1).

func TestRFC9111_Age_AcceptMissUnchanged(t *testing.T) {
	e := newEnv(t, DefaultConfig(), respond(200, "Cache-Control", "max-age=60"))
	r := e.do("GET", "/r")
	if age := r.hdr.Get("Age"); age != "" {
		t.Errorf("origin response gained Age %q", age)
	}
}

func TestRFC9111_Age_RejectHitWithoutAge(t *testing.T) {
	e := newEnv(t, DefaultConfig(), respond(200,
		"Cache-Control", "max-age=60",
		"Date", httpDate(time.Now().Add(-10*time.Second))))
	e.do("GET", "/r")
	r := e.do("GET", "/r")
	if r.hdr.Get("X-Cache") != "HIT" {
		t.Fatalf("X-Cache = %q, want HIT", r.hdr.Get("X-Cache"))
	}
	age, err := strconv.Atoi(r.hdr.Get("Age"))
	if err != nil || age < 10 || age >= 60 {
		t.Errorf("hit Age = %q, want an integer in [10, 60)", r.hdr.Get("Age"))
	}
}

// Stored headers are kept and Date is added if absent (RFC 9111 3.1, RFC 9110 6.6.1).

func TestRFC9111_Date_AcceptAddedWhenAbsent(t *testing.T) {
	// A recorder adds no Date of its own, unlike a real server.
	m := newTestMiddleware(t, DefaultConfig())
	defer m.Close()
	h := m.Wrap(respond(200, "Cache-Control", "max-age=60"))

	before := time.Now().Truncate(time.Second)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/r", nil))
	after := time.Now()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/r", nil))

	if rec.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("X-Cache = %q, want HIT", rec.Header().Get("X-Cache"))
	}
	date, err := http.ParseTime(rec.Header().Get("Date"))
	if err != nil || date.Before(before) || date.After(after) {
		t.Errorf("hit Date = %q, want the time the response was stored", rec.Header().Get("Date"))
	}
	// A hit within the same second cannot tell storage time from hit time,
	// so check the stored response itself.
	stored, _ := m.cache.Get(DefaultKeyGenerator(httptest.NewRequest("GET", "/r", nil)))
	if stored == nil || stored.Headers.Get("Date") != rec.Header().Get("Date") {
		t.Errorf("stored response has no Date, so the hit Date is not the storage time")
	}
}

func TestRFC9111_Date_RejectReplacedOnHit(t *testing.T) {
	stamp := httpDate(time.Now().Add(-10 * time.Second))
	e := newEnv(t, DefaultConfig(), respond(200, "Cache-Control", "max-age=60", "Date", stamp))
	e.do("GET", "/r")
	r := e.do("GET", "/r")
	if r.hdr.Get("Date") != stamp {
		t.Errorf("hit Date = %q, want origin Date %q (X-Cache=%s)", r.hdr.Get("Date"), stamp, r.hdr.Get("X-Cache"))
	}
}

// Vary (RFC 9111 4.1).

func tenantHandler(vary ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for _, v := range vary {
			w.Header().Add("Vary", v)
		}
		w.Header().Set("Cache-Control", "max-age=60")
		fmt.Fprintf(w, "tenant=%s", r.Header.Get("X-Tenant"))
	}
}

func TestRFC9111_Vary_AcceptMatch(t *testing.T) {
	e := newEnv(t, DefaultConfig(), tenantHandler("X-Tenant"))
	e.do("GET", "/r", "X-Tenant", "a")
	r := e.do("GET", "/r", "X-Tenant", "a")
	wantOrigin(t, e, 1, "a matching Vary field should be reused")
	if r.body != "tenant=a" {
		t.Errorf("body = %q, want tenant=a", r.body)
	}
}

func TestRFC9111_Vary_RejectMismatch(t *testing.T) {
	e := newEnv(t, DefaultConfig(), tenantHandler("X-Tenant"))
	e.do("GET", "/r", "X-Tenant", "a")
	r := e.do("GET", "/r", "X-Tenant", "b")
	if r.body != "tenant=b" {
		t.Errorf("tenant b got %q (X-Cache=%s)", r.body, r.hdr.Get("X-Cache"))
	}
}

func TestRFC9111_Vary_RejectStar(t *testing.T) {
	e := newEnv(t, DefaultConfig(), tenantHandler("*"))
	e.do("GET", "/r")
	e.do("GET", "/r")
	wantOrigin(t, e, 2, "Vary: * never matches")
}

func TestRFC9111_Vary_RejectSecondFieldLine(t *testing.T) {
	e := newEnv(t, DefaultConfig(), tenantHandler("Accept", "X-Tenant"))
	e.do("GET", "/r", "Accept", "text/plain", "X-Tenant", "a")
	r := e.do("GET", "/r", "Accept", "text/plain", "X-Tenant", "b")
	if r.body != "tenant=b" {
		t.Errorf("tenant b got %q (X-Cache=%s)", r.body, r.hdr.Get("X-Cache"))
	}
}

func TestRFC9111_Vary_RejectAbsentVsPresent(t *testing.T) {
	t.Run("absent then present", func(t *testing.T) {
		e := newEnv(t, DefaultConfig(), tenantHandler("X-Tenant"))
		e.do("GET", "/r")
		r := e.do("GET", "/r", "X-Tenant", "b")
		if r.body != "tenant=b" {
			t.Errorf("tenant b got %q (X-Cache=%s)", r.body, r.hdr.Get("X-Cache"))
		}
	})
	t.Run("present then absent", func(t *testing.T) {
		e := newEnv(t, DefaultConfig(), tenantHandler("X-Tenant"))
		e.do("GET", "/r", "X-Tenant", "a")
		r := e.do("GET", "/r")
		if r.body != "tenant=" {
			t.Errorf("request without X-Tenant got %q (X-Cache=%s)", r.body, r.hdr.Get("X-Cache"))
		}
	})
}

// The origin reflects X-Forwarded-Host and declares it in Vary. One attacker
// request must not poison the entry for everybody.
func TestRFC9111_Vary_RejectUnkeyedHeaderPoisoning(t *testing.T) {
	e := newEnv(t, DefaultConfig(), func(w http.ResponseWriter, r *http.Request) {
		host := r.Header.Get("X-Forwarded-Host")
		if host == "" {
			host = r.Host
		}
		w.Header().Set("Vary", "X-Forwarded-Host")
		w.Header().Set("Cache-Control", "public, max-age=60")
		fmt.Fprintf(w, `<script src="https://%s/app.js"></script>`, host)
	})
	e.do("GET", "/", "Host", "shop.example", "X-Forwarded-Host", "evil.example")
	r := e.do("GET", "/", "Host", "shop.example")
	if strings.Contains(r.body, "evil.example") {
		t.Errorf("victim got poisoned page %q (X-Cache=%s)", r.body, r.hdr.Get("X-Cache"))
	}
}

// Authorization (RFC 9111 3, 3.5).

func whoami(cc string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", cc)
		fmt.Fprintf(w, "secret for %s", strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	}
}

func TestRFC9111_Authorization_AcceptPublic(t *testing.T) {
	e := newEnv(t, DefaultConfig(), whoami("public, max-age=60"))
	e.do("GET", "/me", "Authorization", "Bearer alice")
	e.do("GET", "/me", "Authorization", "Bearer alice")
	wantOrigin(t, e, 1, "a public response to an authorized request may be reused")
}

func TestRFC9111_Authorization_RejectSameCredential(t *testing.T) {
	e := newEnv(t, DefaultConfig(), whoami("max-age=60"))
	e.do("GET", "/me", "Authorization", "Bearer alice")
	e.do("GET", "/me", "Authorization", "Bearer alice")
	wantOrigin(t, e, 2, "without public, s-maxage or must-revalidate the response must not be stored")
}

func TestRFC9111_Authorization_RejectPathKey(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PathExtractor = PathExtractorFromKey
	e := newEnv(t, cfg, whoami("max-age=60"), func(m *Middleware) {
		m.SetKeyGenerator(KeyWithoutQuery())
	})
	e.do("GET", "/me", "Authorization", "Bearer alice")
	r := e.do("GET", "/me", "Authorization", "Bearer bob")
	if r.body != "secret for bob" {
		t.Errorf("bob got %q (X-Cache=%s)", r.body, r.hdr.Get("X-Cache"))
	}
}

func TestRFC9111_Authorization_RejectCustomPolicy(t *testing.T) {
	e := newEnv(t, DefaultConfig(), whoami("max-age=60"), func(m *Middleware) {
		m.SetCachePolicy(AlwaysCache(time.Minute))
	})
	e.do("GET", "/me", "Authorization", "Bearer alice")
	e.do("GET", "/me", "Authorization", "Bearer alice")
	wantOrigin(t, e, 2, "the Authorization rule applies under any Policy")
}

// The key is the method and target URI, with the authority from Host (RFC 9111 2, 4).

func hostEcho(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=60")
	fmt.Fprintf(w, "site=%s", r.Host)
}

func TestRFC9111_Host_AcceptSameHost(t *testing.T) {
	e := newEnv(t, DefaultConfig(), hostEcho)
	e.do("GET", "/", "Host", "tenant-a.example")
	r := e.do("GET", "/", "Host", "tenant-a.example")
	wantOrigin(t, e, 1, "the same Host should share an entry")
	if r.body != "site=tenant-a.example" {
		t.Errorf("body = %q", r.body)
	}
}

func TestRFC9111_Host_RejectOtherHost(t *testing.T) {
	for _, g := range generators {
		t.Run(g.name, func(t *testing.T) {
			e := newEnv(t, DefaultConfig(), hostEcho, func(m *Middleware) { m.SetKeyGenerator(g.gen) })
			e.do("GET", "/", "Host", "tenant-a.example")
			r := e.do("GET", "/", "Host", "tenant-b.example")
			if r.body != "site=tenant-b.example" {
				t.Errorf("tenant-b.example got %q (X-Cache=%s)", r.body, r.hdr.Get("X-Cache"))
			}
		})
	}
}

// Distinct target URIs never share an entry (RFC 9111 4).

// The middleware wraps a whole mux and the attacker's path spells a header.
func TestRFC9111_Key_RejectComponentCollision(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/users", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `["alice","bob"]`)
	})
	e := newEnv(t, DefaultConfig(), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=60") // the 404 is storable too
		mux.ServeHTTP(w, r)
	})
	a := e.do("GET", "/api/usersAccept-Encoding:gzip")
	v := e.do("GET", "/api/users", "Accept-Encoding", "gzip")
	if a.status != 404 || v.status != 200 {
		t.Errorf("attacker got %d, victim got %d %q (X-Cache=%s), want 404 then 200", a.status, v.status, strings.TrimSpace(v.body), v.hdr.Get("X-Cache"))
	}
}

// ServeMux routes on the escaped path, so /files/a%2Fb and /files/a/b are different resources.
func TestRFC9111_Key_RejectEscapedSlash(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /files/{name}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "file %q", r.PathValue("name"))
	})
	cfg := DefaultConfig()
	cfg.PathExtractor = PathExtractorFromKey
	e := newEnv(t, cfg, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=60") // the 404 is storable too
		mux.ServeHTTP(w, r)
	}, func(m *Middleware) { m.SetKeyGenerator(KeyWithoutQuery()) })
	a := e.do("GET", "/files/a/b")
	v := e.do("GET", "/files/a%2Fb")
	if a.status != 404 || v.status != 200 {
		t.Errorf("attacker got %d, victim got %d %q (X-Cache=%s), want 404 then 200", a.status, v.status, strings.TrimSpace(v.body), v.hdr.Get("X-Cache"))
	}
}

// Conditional requests (RFC 9111 3, 4.3.2, 4.3.4).

func fileHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "max-age=60")
	w.Header().Set("ETag", `"v1"`)
	http.ServeContent(w, r, "x.txt", time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), strings.NewReader("hello world"))
}

func TestRFC9111_Conditional_AcceptOriginOrFull(t *testing.T) {
	e := newEnv(t, DefaultConfig(), fileHandler)
	e.do("GET", "/f")
	r := e.do("GET", "/f", "If-None-Match", `"v1"`)
	if (r.status != 304 || r.body != "") && (r.status != 200 || r.body != "hello world") {
		t.Errorf("matching If-None-Match got %d %q, want 304 or the full 200", r.status, r.body)
	}
}

func TestRFC9111_Conditional_RejectIfNoneMatch(t *testing.T) {
	e := newEnv(t, DefaultConfig(), fileHandler)
	e.do("GET", "/f", "If-None-Match", `"v1"`)
	r := e.do("GET", "/f")
	if r.status != 200 || r.body != "hello world" {
		t.Errorf("unconditional GET got %d %q (X-Cache=%s)", r.status, r.body, r.hdr.Get("X-Cache"))
	}
}

func TestRFC9111_Conditional_RejectIfModifiedSince(t *testing.T) {
	e := newEnv(t, DefaultConfig(), fileHandler)
	e.do("GET", "/f", "If-Modified-Since", httpDate(time.Now()))
	r := e.do("GET", "/f")
	if r.status != 200 || r.body != "hello world" {
		t.Errorf("unconditional GET got %d %q (X-Cache=%s)", r.status, r.body, r.hdr.Get("X-Cache"))
	}
}

// A 206 never answers a full request (RFC 9111 3.3).

func TestRFC9111_Range_AcceptAfterFull(t *testing.T) {
	e := newEnv(t, DefaultConfig(), fileHandler)
	e.do("GET", "/f")
	r := e.do("GET", "/f", "Range", "bytes=0-4")
	if (r.status != 206 || r.body != "hello") && (r.status != 200 || r.body != "hello world") {
		t.Errorf("Range after full GET got %d %q", r.status, r.body)
	}
}

func TestRFC9111_Range_Reject206WhenConfigured(t *testing.T) {
	with206 := DefaultConfig()
	with206.CacheableStatus = append(with206.CacheableStatus, 206)
	for name, cfg := range map[string]Config{"default": DefaultConfig(), "206 cacheable": with206} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, cfg, fileHandler)
			e.do("GET", "/f", "Range", "bytes=0-4")
			r := e.do("GET", "/f")
			if r.status != 200 || r.body != "hello world" {
				t.Errorf("full GET after Range got %d %q (X-Cache=%s)", r.status, r.body, r.hdr.Get("X-Cache"))
			}
		})
	}
}

// Heuristic freshness only for heuristically cacheable statuses or public (RFC 9111 4.2.2).

func statusHandler(cc string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		code, _ := strconv.Atoi(r.URL.Query().Get("s"))
		if cc != "" {
			w.Header().Set("Cache-Control", cc)
		}
		if code >= 300 && code < 400 {
			w.Header().Set("Location", "/login")
		}
		w.WriteHeader(code)
		fmt.Fprint(w, "x")
	}
}

func TestRFC9111_Heuristic_AcceptPublic(t *testing.T) {
	for _, code := range []int{200, 301, 302, 404, 410} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			e := newEnv(t, DefaultConfig(), statusHandler("public"))
			p := fmt.Sprintf("/s?s=%d", code)
			e.do("GET", p)
			e.do("GET", p)
			wantOrigin(t, e, 1, "a public response may be stored with DefaultTTL")
		})
	}
}

func TestRFC9111_Heuristic_RejectNonHeuristicStatus(t *testing.T) {
	for _, code := range []int{201, 302} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			e := newEnv(t, DefaultConfig(), statusHandler(""))
			p := fmt.Sprintf("/s?s=%d", code)
			e.do("GET", p)
			e.do("GET", p)
			wantOrigin(t, e, 2, "no explicit freshness and a status that is not heuristically cacheable")
		})
	}
}

// An unsafe method invalidates the target URI (RFC 9111 4.4).

func itemHandler(writeStatus int) http.HandlerFunc {
	var version atomic.Int64
	version.Store(1)
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			w.Header().Set("Cache-Control", "max-age=60")
			fmt.Fprintf(w, "v%d", version.Load())
		default:
			if writeStatus < 400 {
				version.Add(1)
			}
			w.WriteHeader(writeStatus)
		}
	}
}

func TestRFC9111_Unsafe_AcceptErrorKeepsEntry(t *testing.T) {
	e := newEnv(t, DefaultConfig(), itemHandler(500))
	e.do("GET", "/item")
	e.do("POST", "/item")
	r := e.do("GET", "/item")
	if r.body != "v1" || r.hdr.Get("X-Cache") != "HIT" {
		t.Errorf("GET after failed POST got %q (X-Cache=%s), want the cached v1", r.body, r.hdr.Get("X-Cache"))
	}
}

func TestRFC9111_Unsafe_RejectStaleGet(t *testing.T) {
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		t.Run(method, func(t *testing.T) {
			e := newEnv(t, DefaultConfig(), itemHandler(200))
			e.do("GET", "/item")
			e.do(method, "/item")
			r := e.do("GET", "/item")
			if r.body != "v2" {
				t.Errorf("GET after successful %s got %q (X-Cache=%s), want v2", method, r.body, r.hdr.Get("X-Cache"))
			}
		})
	}
}

func TestRFC9111_Unsafe_RejectStaleHead(t *testing.T) {
	e := newEnv(t, DefaultConfig(), itemHandler(200))
	e.do("HEAD", "/item")
	e.do("POST", "/item")
	e.do("HEAD", "/item")
	wantOrigin(t, e, 3, "a successful POST must invalidate the stored HEAD response")
}

// Unsafe methods are not stored and a HEAD never answers a GET (RFC 9111 3, 4).

func TestRFC9111_Method_AcceptHeadHit(t *testing.T) {
	e := newEnv(t, DefaultConfig(), respond(200, "Cache-Control", "max-age=60"))
	e.do("HEAD", "/r")
	r := e.do("HEAD", "/r")
	wantOrigin(t, e, 1, "HEAD is cacheable")
	if r.hdr.Get("X-Cache") != "HIT" {
		t.Errorf("X-Cache = %q, want HIT", r.hdr.Get("X-Cache"))
	}
}

func TestRFC9111_Method_RejectPostStored(t *testing.T) {
	e := newEnv(t, DefaultConfig(), respond(200, "Cache-Control", "max-age=60"))
	e.do("POST", "/r")
	e.do("POST", "/r")
	wantOrigin(t, e, 2, "POST responses are not stored")
}

func TestRFC9111_Method_RejectGetFromHead(t *testing.T) {
	e := newEnv(t, DefaultConfig(), respond(200, "Cache-Control", "max-age=60"))
	e.do("HEAD", "/r")
	r := e.do("GET", "/r")
	if r.body == "" {
		t.Errorf("GET after a stored HEAD got an empty body (X-Cache=%s)", r.hdr.Get("X-Cache"))
	}
}

// By default only responses with explicit freshness, or public ones, are stored.

func TestHTTPCache_Default_AcceptExplicitFreshness(t *testing.T) {
	now := time.Now()
	for name, kv := range map[string][]string{
		"max-age":  {"Cache-Control", "max-age=60"},
		"s-maxage": {"Cache-Control", "s-maxage=60"},
		"Expires":  {"Date", httpDate(now), "Expires", httpDate(now.Add(time.Minute))},
		"public":   {"Cache-Control", "public"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, DefaultConfig(), respond(200, kv...))
			e.do("GET", "/r")
			e.do("GET", "/r")
			wantOrigin(t, e, 1, "a response with explicit freshness should be reused")
		})
	}
}

func TestHTTPCache_Default_RejectNoFreshness(t *testing.T) {
	for _, code := range []int{200, 301, 404, 410} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			e := newEnv(t, DefaultConfig(), statusHandler(""))
			p := fmt.Sprintf("/s?s=%d", code)
			e.do("GET", p)
			e.do("GET", p)
			wantOrigin(t, e, 2, "a response without explicit freshness is not stored")
		})
	}
}

// Cookie session auth and no Cache-Control, a common shape for Go web apps.
func TestHTTPCache_Default_RejectCookiePage(t *testing.T) {
	e := newEnv(t, DefaultConfig(), func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("session")
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		fmt.Fprintf(w, "account page of %s", c.Value)
	})
	t.Run("account page served to another user", func(t *testing.T) {
		e.do("GET", "/account", "Cookie", "session=alice")
		r := e.do("GET", "/account", "Cookie", "session=bob")
		if r.body != "account page of bob" {
			t.Errorf("bob got %q (X-Cache=%s)", r.body, r.hdr.Get("X-Cache"))
		}
	})
	t.Run("login redirect served to a logged-in user", func(t *testing.T) {
		e.do("GET", "/settings")
		r := e.do("GET", "/settings", "Cookie", "session=carol")
		if r.status != 200 {
			t.Errorf("carol got %d to %q (X-Cache=%s)", r.status, r.hdr.Get("Location"), r.hdr.Get("X-Cache"))
		}
	})
}

// Responses with Set-Cookie are not stored.

func TestHTTPCache_SetCookie_AcceptWithout(t *testing.T) {
	e := newEnv(t, DefaultConfig(), respond(200, "Cache-Control", "public, max-age=60"))
	e.do("GET", "/landing")
	e.do("GET", "/landing")
	wantOrigin(t, e, 1, "a public response without Set-Cookie should be reused")
}

func TestHTTPCache_SetCookie_RejectReplay(t *testing.T) {
	var n atomic.Int64
	e := newEnv(t, DefaultConfig(), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=60")
		http.SetCookie(w, &http.Cookie{Name: "session", Value: fmt.Sprintf("sess-%d", n.Add(1)), HttpOnly: true})
		fmt.Fprint(w, "welcome")
	})
	a := e.do("GET", "/landing")
	b := e.do("GET", "/landing")
	if a.hdr.Get("Set-Cookie") == b.hdr.Get("Set-Cookie") {
		t.Errorf("client B received client A's cookie %q (X-Cache=%s)", b.hdr.Get("Set-Cookie"), b.hdr.Get("X-Cache"))
	}
}

// Request no-store is honoured (a MAY in RFC 9111 5.2.1.5).

func TestRFC9111_ReqNoStore_AcceptStoredEntryServed(t *testing.T) {
	e := newEnv(t, DefaultConfig(), respond(200, "Cache-Control", "max-age=60"))
	e.do("GET", "/r")
	r := e.do("GET", "/r", "Cache-Control", "no-store")
	wantOrigin(t, e, 1, "request no-store forbids storing, not reusing a stored response")
	if r.hdr.Get("X-Cache") != "HIT" {
		t.Errorf("X-Cache = %q, want HIT", r.hdr.Get("X-Cache"))
	}
}

func TestRFC9111_ReqNoStore_RejectStored(t *testing.T) {
	for name, kv := range map[string][]string{
		"one line":    {"Cache-Control", "no-store"},
		"second line": {"Cache-Control", "max-stale", "Cache-Control", "no-store"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, DefaultConfig(), respond(200, "Cache-Control", "max-age=60"))
			e.do("GET", "/r", kv...)
			e.do("GET", "/r")
			wantOrigin(t, e, 2, "the response to a no-store request must not be stored")
		})
	}
}

// Hits carry X-Cache and Age, not X-Cache-Date or X-Cache-Age.

func TestHTTPCache_Hit_AcceptXCacheHit(t *testing.T) {
	e := newEnv(t, DefaultConfig(), respond(200, "Cache-Control", "max-age=60"))
	e.do("GET", "/r")
	r := e.do("GET", "/r")
	if r.hdr.Get("X-Cache") != "HIT" {
		t.Errorf("X-Cache = %q, want HIT", r.hdr.Get("X-Cache"))
	}
}

func TestHTTPCache_Hit_RejectDiagnosticHeaders(t *testing.T) {
	e := newEnv(t, DefaultConfig(), respond(200, "Cache-Control", "max-age=60"))
	e.do("GET", "/r")
	r := e.do("GET", "/r")
	for _, name := range []string{"X-Cache-Date", "X-Cache-Age"} {
		if v := r.hdr.Get(name); v != "" {
			t.Errorf("hit has %s: %q", name, v)
		}
	}
}

// Rules about the request judge it as the client sent it, not as the handler
// left it.

func TestRFC9111_Authorization_RejectHandlerDeletesHeader(t *testing.T) {
	// An auth middleware behind the cache consumes Authorization and deletes it.
	e := newEnv(t, DefaultConfig(), func(w http.ResponseWriter, r *http.Request) {
		user := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		r.Header.Del("Authorization")
		w.Header().Set("Cache-Control", "max-age=60")
		fmt.Fprintf(w, "secret for %s", user)
	})
	e.do("GET", "/me", "Authorization", "Bearer alice")
	r := e.do("GET", "/me", "Authorization", "Bearer bob")
	if r.body != "secret for bob" {
		t.Errorf("bob got %q (X-Cache=%s)", r.body, r.hdr.Get("X-Cache"))
	}
}

func TestRFC9111_Vary_RejectHandlerRewritesHeader(t *testing.T) {
	// The handler replaces X-Tenant with the tenant it derives from the session.
	e := newEnv(t, DefaultConfig(), func(w http.ResponseWriter, r *http.Request) {
		tenant := "public"
		if r.Header.Get("Cookie") == "session=acme-admin" {
			tenant = "acme"
		}
		r.Header.Set("X-Tenant", tenant)
		w.Header().Set("Vary", "X-Tenant")
		w.Header().Set("Cache-Control", "public, max-age=60")
		fmt.Fprintf(w, "dashboard of %s", tenant)
	})
	e.do("GET", "/dash", "Cookie", "session=acme-admin")
	r := e.do("GET", "/dash", "X-Tenant", "acme")
	if r.body != "dashboard of public" {
		t.Errorf("client without a session got %q (X-Cache=%s)", r.body, r.hdr.Get("X-Cache"))
	}
}

func TestRFC9111_Unsafe_RejectHandlerRewritesTarget(t *testing.T) {
	var version atomic.Int64
	version.Store(1)
	e := newEnv(t, DefaultConfig(), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Cache-Control", "max-age=60")
			fmt.Fprintf(w, "v%d", version.Load())
			return
		}
		r.URL.Path = "/rewritten" // a router that rewrites the path in place
		version.Add(1)
	})
	e.do("GET", "/item")
	e.do("POST", "/item")
	r := e.do("GET", "/item")
	if r.body != "v2" {
		t.Errorf("GET after successful POST got %q (X-Cache=%s), want v2", r.body, r.hdr.Get("X-Cache"))
	}
}

// Response fields set under non-canonical keys reach the wire, so the rules
// must see them too.

func TestHTTPCache_SetCookie_RejectLowercaseKey(t *testing.T) {
	var n atomic.Int64
	e := newEnv(t, DefaultConfig(), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=60")
		w.Header()["set-cookie"] = []string{fmt.Sprintf("session=%d", n.Add(1))}
		fmt.Fprint(w, "welcome")
	})
	e.do("GET", "/r")
	b := e.do("GET", "/r")
	wantOrigin(t, e, 2, "a response with set-cookie must not be stored")
	if b.hdr.Get("Set-Cookie") != "session=2" {
		t.Errorf("second client got Set-Cookie %q (X-Cache=%s)", b.hdr.Get("Set-Cookie"), b.hdr.Get("X-Cache"))
	}
}

func TestRFC9111_Private_RejectLowercaseKey(t *testing.T) {
	e := newEnv(t, DefaultConfig(), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header()["cache-control"] = []string{"private"}
		fmt.Fprint(w, "x")
	})
	e.do("GET", "/r")
	e.do("GET", "/r")
	wantOrigin(t, e, 2, "private under a cache-control key must prevent storage")
}

func TestRFC9111_NoStore_RejectLowercaseKeyCustomPolicy(t *testing.T) {
	e := newEnv(t, DefaultConfig(), func(w http.ResponseWriter, r *http.Request) {
		w.Header()["cache-control"] = []string{"no-store"}
		fmt.Fprint(w, "x")
	}, func(m *Middleware) { m.SetCachePolicy(AlwaysCache(time.Minute)) })
	e.do("GET", "/r")
	e.do("GET", "/r")
	wantOrigin(t, e, 2, "no-store under a cache-control key must prevent storage")
}

func TestRFC9111_Vary_RejectLowercaseKey(t *testing.T) {
	e := newEnv(t, DefaultConfig(), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=60")
		w.Header()["vary"] = []string{"X-Tenant"}
		fmt.Fprintf(w, "tenant=%s", r.Header.Get("X-Tenant"))
	})
	e.do("GET", "/r", "X-Tenant", "a")
	r := e.do("GET", "/r", "X-Tenant", "b")
	if r.body != "tenant=b" {
		t.Errorf("tenant b got %q (X-Cache=%s)", r.body, r.hdr.Get("X-Cache"))
	}
}

// A field line with an empty value still means the field is present.

func TestHTTPCache_SetCookie_RejectEmptyValue(t *testing.T) {
	for name, kv := range map[string][]string{
		"empty then real": {"Set-Cookie", "", "Set-Cookie", "session=alice"},
		"lone empty":      {"Set-Cookie", ""},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, DefaultConfig(), respond(200, append([]string{"Cache-Control", "public, max-age=60"}, kv...)...))
			e.do("GET", "/r")
			e.do("GET", "/r")
			wantOrigin(t, e, 2, "a response with any Set-Cookie line must not be stored")
		})
	}
}

func TestRFC9111_Authorization_RejectEmptyValue(t *testing.T) {
	for name, kv := range map[string][]string{
		"empty then real": {"Authorization", "", "Authorization", "Bearer alice"},
		"lone empty":      {"Authorization", ""},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, DefaultConfig(), whoami("max-age=60"))
			e.do("GET", "/me", kv...)
			e.do("GET", "/me", kv...)
			wantOrigin(t, e, 2, "a request with any Authorization line makes the response unstorable")
		})
	}
}

func TestRFC9111_Range_RejectEmptyFirstLine(t *testing.T) {
	e := newEnv(t, DefaultConfig(), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=60")
		fmt.Fprintf(w, "range=%s", strings.Join(r.Header.Values("Range"), ","))
	})
	e.do("GET", "/f", "Range", "", "Range", "bytes=0-3")
	r := e.do("GET", "/f")
	if r.body != "range=" {
		t.Errorf("plain GET got %q (X-Cache=%s), want the answer to a request without Range", r.body, r.hdr.Get("X-Cache"))
	}
}

// A 206 is never stored, whatever the policy (RFC 9111 3.3).
func TestRFC9111_Range_Reject206UnderAnyPolicy(t *testing.T) {
	partial := respond(206, "Cache-Control", "max-age=60", "Content-Range", "bytes 0-5/100")
	with206 := DefaultConfig()
	with206.CacheableStatus = append(with206.CacheableStatus, 206)
	for name, tc := range map[string]struct {
		cfg   Config
		setup []func(*Middleware)
	}{
		"AlwaysCache":            {DefaultConfig(), []func(*Middleware){func(m *Middleware) { m.SetCachePolicy(AlwaysCache(time.Minute)) }}},
		"206 in CacheableStatus": {with206, nil},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, tc.cfg, partial, tc.setup...)
			e.do("GET", "/f")
			r := e.do("GET", "/f")
			wantOrigin(t, e, 2, "a 206 must not be stored")
			if r.hdr.Get("X-Cache") == "HIT" {
				t.Errorf("plain GET got a stored %d", r.status)
			}
		})
	}
}

// Conditionals go to the origin. A failing If-Match gets its 412, and a 304 is
// not stored even when CacheableStatus allows it.
func TestRFC9111_Conditional_RejectIfMatchFromCache(t *testing.T) {
	e := newEnv(t, DefaultConfig(), fileHandler)
	e.do("GET", "/f")
	r := e.do("GET", "/f", "If-Match", `"v2"`)
	if r.status != http.StatusPreconditionFailed {
		t.Errorf("failing If-Match got %d %q (X-Cache=%s), want 412 from the origin", r.status, r.body, r.hdr.Get("X-Cache"))
	}
}

func TestRFC9111_Conditional_Reject304WhenConfigured(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CacheableStatus = append(cfg.CacheableStatus, 304)
	e := newEnv(t, cfg, fileHandler)
	e.do("GET", "/f", "If-None-Match", `"v1"`)
	r := e.do("GET", "/f")
	if r.status != 200 || r.body != "hello world" {
		t.Errorf("unconditional GET got %d %q (X-Cache=%s)", r.status, r.body, r.hdr.Get("X-Cache"))
	}
}

// OPTIONS and TRACE are safe and do not invalidate.
func TestRFC9111_Unsafe_AcceptSafeMethodKeepsEntry(t *testing.T) {
	for _, method := range []string{"OPTIONS", "TRACE"} {
		t.Run(method, func(t *testing.T) {
			e := newEnv(t, DefaultConfig(), itemHandler(200))
			e.do("GET", "/item")
			e.do(method, "/item")
			r := e.do("GET", "/item")
			if r.hdr.Get("X-Cache") != "HIT" {
				t.Errorf("GET after %s got %q (X-Cache=%s), want a hit", method, r.body, r.hdr.Get("X-Cache"))
			}
		})
	}
}

// A 3xx is a non-error status, so POST then 303 invalidates.
func TestRFC9111_Unsafe_RejectStaleGetAfter303(t *testing.T) {
	e := newEnv(t, DefaultConfig(), itemHandler(http.StatusSeeOther))
	e.do("GET", "/item")
	e.do("POST", "/item")
	r := e.do("GET", "/item")
	if r.body != "v2" {
		t.Errorf("GET after POST 303 got %q (X-Cache=%s), want v2", r.body, r.hdr.Get("X-Cache"))
	}
}

// Every storage rule holds under a custom policy.
func TestHTTPCache_StorageRules_RejectCustomPolicy(t *testing.T) {
	for name, tc := range map[string]struct {
		h   http.HandlerFunc
		req []string
	}{
		"private":          {h: respond(200, "Cache-Control", "private")},
		"no-cache":         {h: respond(200, "Cache-Control", "no-cache")},
		"Set-Cookie":       {h: respond(200, "Set-Cookie", "s=1")},
		"Vary *":           {h: respond(200, "Vary", "*")},
		"request no-store": {h: respond(200), req: []string{"Cache-Control", "no-store"}},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, DefaultConfig(), tc.h, func(m *Middleware) { m.SetCachePolicy(AlwaysCache(time.Minute)) })
			e.do("GET", "/r", tc.req...)
			e.do("GET", "/r")
			wantOrigin(t, e, 2, name+" must prevent storage under any Policy")
		})
	}
}

// Each directive RFC 9111 3.5 names allows storing an authorized response.
func TestRFC9111_Authorization_AcceptEachDirective(t *testing.T) {
	for _, cc := range []string{"public, max-age=60", "s-maxage=60", "must-revalidate, max-age=60"} {
		t.Run(cc, func(t *testing.T) {
			e := newEnv(t, DefaultConfig(), whoami(cc))
			e.do("GET", "/me", "Authorization", "Bearer alice")
			e.do("GET", "/me", "Authorization", "Bearer alice")
			wantOrigin(t, e, 1, cc+" allows a shared cache to store an authorized response")
		})
	}
}

// A list-valued Age uses its first member, and an overflowing Age counts as
// 2^31 (RFC 9111 5.1, 1.2.2).
func TestRFC9111_Age_RejectListAndOverflow(t *testing.T) {
	for _, age := range []string{"120, 30", "99999999999999999999"} {
		t.Run(age, func(t *testing.T) {
			e := newEnv(t, DefaultConfig(), respond(200, "Cache-Control", "max-age=60", "Age", age))
			e.do("GET", "/r")
			e.do("GET", "/r")
			wantOrigin(t, e, 2, "Age "+age+" exceeds max-age=60")
		})
	}
}

func TestRFC9111_Age_RejectNegativeOnHit(t *testing.T) {
	// AlwaysCache stores the response although its Date makes it very old.
	e := newEnv(t, DefaultConfig(), respond(200, "Date", "Mon, 01 Jan 0001 00:00:00 GMT"), func(m *Middleware) {
		m.SetCachePolicy(AlwaysCache(time.Minute))
	})
	e.do("GET", "/r")
	r := e.do("GET", "/r")
	if r.hdr.Get("X-Cache") != "HIT" {
		t.Fatalf("X-Cache = %q, want HIT", r.hdr.Get("X-Cache"))
	}
	age, err := strconv.ParseInt(r.hdr.Get("Age"), 10, 64)
	if err != nil || age < 1<<31 || age > 1<<31+60 {
		t.Errorf("hit Age = %q, want 2^31 seconds plus the time since storage", r.hdr.Get("Age"))
	}
}

// Vary compares every request field line, and an empty value differs from absent.
func TestRFC9111_Vary_RejectRequestFieldLines(t *testing.T) {
	for name, reqs := range map[string][2][]string{
		"two lines then one": {{"X-Tenant", "a", "X-Tenant", "b"}, {"X-Tenant", "a"}},
		"empty then absent":  {{"X-Tenant", ""}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, DefaultConfig(), tenantHandler("X-Tenant"))
			e.do("GET", "/r", reqs[0]...)
			r := e.do("GET", "/r", reqs[1]...)
			if r.hdr.Get("X-Cache") == "HIT" {
				t.Errorf("request with different X-Tenant field lines was served from cache")
			}
		})
	}
}

// Hop-by-hop fields describe one connection and are not replayed on a hit
// (RFC 9110 7.6.1).
func TestRFC9111_HopByHop_RejectReplayOnHit(t *testing.T) {
	m := newTestMiddleware(t, DefaultConfig())
	defer m.Close()
	h := m.Wrap(respond(200,
		"Cache-Control", "max-age=60",
		"Connection", "close, X-Hop",
		"Connection", "x-hop2",
		"X-Hop", "secret",
		"X-Hop2", "secret2",
		"Keep-Alive", "timeout=5",
		"Proxy-Connection", "keep-alive",
		"X-End", "kept"))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/r", nil))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/r", nil))

	if rec.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("X-Cache = %q, want HIT", rec.Header().Get("X-Cache"))
	}
	for _, name := range []string{"Connection", "X-Hop", "X-Hop2", "Keep-Alive", "Proxy-Connection"} {
		if v := rec.Header().Values(name); len(v) > 0 {
			t.Errorf("hit replayed %s: %q", name, v)
		}
	}
	if v := rec.Header().Get("X-End"); v != "kept" {
		t.Errorf("hit X-End = %q, want kept", v)
	}
}

func TestRFC9111_ReqNoStore_RejectHandlerDeletesHeader(t *testing.T) {
	// A handler that strips the request's Cache-Control before it runs its logic.
	e := newEnv(t, DefaultConfig(), func(w http.ResponseWriter, r *http.Request) {
		r.Header.Del("Cache-Control")
		w.Header().Set("Cache-Control", "max-age=60")
		fmt.Fprint(w, "x")
	})
	e.do("GET", "/r", "Cache-Control", "no-store")
	e.do("GET", "/r")
	wantOrigin(t, e, 2, "the response to a no-store request must not be stored")
}

// Fields sent under two spellings of one name reach the client as one field,
// so a hit replays every value.
func TestRFC9111_Hit_AcceptNonCanonicalKeys(t *testing.T) {
	e := newEnv(t, DefaultConfig(), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set("X-A", "1")
		w.Header()["x-a"] = []string{"2"}
		fmt.Fprint(w, "x")
	})
	miss := e.do("GET", "/r")
	hit := e.do("GET", "/r")
	if hit.hdr.Get("X-Cache") != "HIT" {
		t.Fatalf("X-Cache = %q, want HIT", hit.hdr.Get("X-Cache"))
	}
	if got, want := slices.Sorted(slices.Values(hit.hdr.Values("X-A"))), slices.Sorted(slices.Values(miss.hdr.Values("X-A"))); !slices.Equal(got, want) {
		t.Errorf("hit X-A = %q, want %q as sent on the miss", got, want)
	}
}

// Keys that differ only in case merge in the order net/http writes them on the
// wire (sorted by key), so first-occurrence rules agree with what clients see.
func TestResponseWriter_MergesCollidingKeysInWireOrder(t *testing.T) {
	want := []string{"max-age=0", "max-age=60", "max-age=600"}
	for range 50 {
		rw := newResponseWriter(httptest.NewRecorder(), "", 0, false)
		rw.Header()["cache-control"] = []string{"max-age=600"}
		rw.Header()["Cache-Control"] = []string{"max-age=60"}
		rw.Header()["CACHE-CONTROL"] = []string{"max-age=0"}
		rw.WriteHeader(http.StatusOK)
		if got := rw.headers["Cache-Control"]; !slices.Equal(got, want) {
			t.Fatalf("Cache-Control = %q, want %q (sorted key order)", got, want)
		}
	}
}
