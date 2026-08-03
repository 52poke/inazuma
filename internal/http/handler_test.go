package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/52poke/inazuma/internal/cache"
)

func TestWriteObjectRestoresSafeHeadersAndControlsVary(t *testing.T) {
	recorder := httptest.NewRecorder()
	obj := cache.Object{
		Body:        []byte("page"),
		ContentType: "text/html; charset=utf-8",
		Headers: map[string]string{
			"Cache-Control":           "public, max-age=60",
			"Content-Language":        "zh-hant",
			"Content-Security-Policy": "default-src 'self'",
			"Vary":                    "Cookie",
		},
	}

	writeObject(recorder, obj, "HIT")

	result := recorder.Result()
	if got, want := result.Header.Get("Vary"), "Accept-Language"; got != want {
		t.Fatalf("Vary = %q, want %q", got, want)
	}
	if got, want := result.Header.Get("Content-Language"), "zh-hant"; got != want {
		t.Fatalf("Content-Language = %q, want %q", got, want)
	}
	if got, want := result.Header.Get("Content-Security-Policy"), "default-src 'self'"; got != want {
		t.Fatalf("Content-Security-Policy = %q, want %q", got, want)
	}
}

func TestCacheResponseHeadersUsesAllowlist(t *testing.T) {
	headers := http.Header{
		"Cache-Control":           {"public, max-age=60"},
		"Content-Security-Policy": {"default-src 'self'"},
		"Set-Cookie":              {"session=secret"},
		"Vary":                    {"Cookie"},
	}

	cached := CacheResponseHeaders(headers)

	if got := cached["Cache-Control"]; got != "public, max-age=60" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if _, ok := cached["Set-Cookie"]; ok {
		t.Fatal("Set-Cookie must not be cached")
	}
	if _, ok := cached["Vary"]; ok {
		t.Fatal("upstream Vary must not be cached")
	}
}

func TestAuthorizationBypassesCache(t *testing.T) {
	h := &Handler{}
	r := httptest.NewRequest(http.MethodGet, "/wiki/Page", nil)
	r.Header.Set("Authorization", "Bearer secret")

	if !h.shouldBypassCache(r) {
		t.Fatal("request with Authorization should bypass cache")
	}
}
