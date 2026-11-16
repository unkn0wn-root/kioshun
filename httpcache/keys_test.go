package httpcache

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

var generators = []struct {
	name string
	gen  KeyGenerator
}{
	{"DefaultKeyGenerator", DefaultKeyGenerator},
	{"KeyWithVaryHeaders", KeyWithVaryHeaders([]string{"Accept"})},
	{"KeyWithoutQuery", KeyWithoutQuery()},
	{"KeyWithoutQueryHashed", KeyWithoutQueryHashed()},
	{"KeyWithUserID", KeyWithUserID("X-User-ID")},
	{"PathBasedKeyGenerator", PathBasedKeyGenerator},
}

// keyRequest builds a server-side request: r.URL holds only the path and
// query, and r.Host carries the authority. kv are header name/value pairs.
func keyRequest(target, host string, kv ...string) *http.Request {
	r := httptest.NewRequest("GET", target, nil)
	r.Host = host
	for i := 0; i+1 < len(kv); i += 2 {
		r.Header.Add(kv[i], kv[i+1])
	}
	return r
}

func TestKeyGenerator_HostInKey(t *testing.T) {
	for _, g := range generators {
		t.Run(g.name, func(t *testing.T) {
			a := keyRequest("/p", "a.example")
			b := keyRequest("/p", "b.example")
			if g.gen(a) == g.gen(b) {
				t.Errorf("hosts a.example and b.example share key %q", g.gen(a))
			}
		})
	}
}

func TestKeyGenerator_AcceptEqualRequests(t *testing.T) {
	kv := []string{"Accept", "text/html", "Authorization", "Bearer a", "X-User-ID", "1"}
	for _, g := range generators {
		t.Run(g.name, func(t *testing.T) {
			a := keyRequest("/p/x?q=1", "a.example", kv...)
			b := keyRequest("/p/x?q=1", "a.example", kv...)
			if g.gen(a) != g.gen(b) {
				t.Errorf("equal requests got keys %q and %q", g.gen(a), g.gen(b))
			}
		})
	}
}

// A path or header value must not spell another key component.
func TestKeyGenerator_RejectComponentCollision(t *testing.T) {
	type collision struct {
		name string
		gen  KeyGenerator
		a, b *http.Request
	}
	ab := KeyWithVaryHeaders([]string{"A", "B"})
	tests := []collision{
		{"DefaultKeyGenerator/path spells header", DefaultKeyGenerator,
			keyRequest("/api/usersAccept-Encoding:gzip", "h"), keyRequest("/api/users", "h", "Accept-Encoding", "gzip")},
		{"KeyWithVaryHeaders/path spells header", ab,
			keyRequest("/pA:1", "h"), keyRequest("/p", "h", "A", "1")},
		{"KeyWithVaryHeaders/header spells header", ab,
			keyRequest("/p", "h", "A", "1B:2"), keyRequest("/p", "h", "A", "1", "B", "2")},
		{"KeyWithoutQueryHashed/path spells header", KeyWithoutQueryHashed(),
			keyRequest("/pAccept:x", "h"), keyRequest("/p", "h", "Accept", "x")},
		{"KeyWithoutQueryHashed/header spells header", KeyWithoutQueryHashed(),
			keyRequest("/p", "h", "Accept", "xAccept-Encoding:gzip"), keyRequest("/p", "h", "Accept", "x", "Accept-Encoding", "gzip")},
		{"KeyWithUserID/path spells header", KeyWithUserID("X-User-ID"),
			keyRequest("/puser:1", "h"), keyRequest("/p", "h", "X-User-ID", "1")},
		{"KeyWithUserID/path spells header, both set", KeyWithUserID("X-User-ID"),
			keyRequest("/puser:1", "h", "X-User-ID", "x"), keyRequest("/p", "h", "X-User-ID", "1user:x")},
	}
	// With no separator, or one that a header value can contain, one value
	// can spell the next field.
	for _, g := range []struct {
		name          string
		gen           KeyGenerator
		first, second string
	}{
		{"KeyWithVaryHeaders", ab, "A", "B"},
		{"KeyWithoutQueryHashed", KeyWithoutQueryHashed(), "Accept", "Accept-Encoding"},
	} {
		for _, sep := range []string{"", " ", "|", ";", "\t"} {
			tests = append(tests, collision{
				fmt.Sprintf("%s/value spells next field with separator %q", g.name, sep), g.gen,
				keyRequest("/p", "h", g.first, "1"+sep+g.second+": 2"),
				keyRequest("/p", "h", g.first, "1", g.second, "2"+sep+g.second+": "),
			})
		}
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if ka, kb := tt.gen(tt.a), tt.gen(tt.b); ka == kb {
				t.Errorf("distinct requests share key %q", ka)
			}
		})
	}
}

// ServeMux routes /files/a%2Fb and /files/a/b differently, so keys must differ.
func TestKeyGenerator_RejectEscapedSlash(t *testing.T) {
	for _, g := range generators {
		t.Run(g.name, func(t *testing.T) {
			a := keyRequest("/files/a%2Fb", "h")
			b := keyRequest("/files/a/b", "h")
			if g.gen(a) == g.gen(b) {
				t.Errorf("/files/a%%2Fb and /files/a/b share key %q", g.gen(a))
			}
		})
	}
}

func TestPathExtractorFromKey(t *testing.T) {
	for key, want := range map[string]string{
		"GET:/api/users":                 "/api/users",
		"GET:example.com/api/users":      "/api/users",
		"GET:example.com:8080/api/users": "/api/users",
		"GET:[::1]:8080/api/users":       "/api/users",
		"/plain":                         "/plain",
	} {
		if got := PathExtractorFromKey(key); got != want {
			t.Errorf("PathExtractorFromKey(%q) = %q, want %q", key, got, want)
		}
	}
}
