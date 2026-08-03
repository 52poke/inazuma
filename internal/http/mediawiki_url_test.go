package httpx

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/52poke/inazuma/internal/cache"
	"github.com/52poke/inazuma/internal/config"
	"github.com/52poke/inazuma/internal/lang"
	"github.com/52poke/inazuma/internal/mw"
)

type fakeRoundTripper func(*http.Request) (*http.Response, error)

func (f fakeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type fakeCacheLock struct{}

func (*fakeCacheLock) Unlock(context.Context) error { return nil }

type memoryStore struct {
	objects map[string]cache.Object
	puts    []string
}

func newMemoryStore() *memoryStore {
	return &memoryStore{objects: make(map[string]cache.Object)}
}

func (s *memoryStore) Get(_ context.Context, key string) (cache.Object, error) {
	obj, ok := s.objects[key]
	if !ok {
		return cache.Object{}, cache.ErrNotFound
	}
	return obj, nil
}

func (s *memoryStore) Put(_ context.Context, key string, obj cache.Object) error {
	s.objects[key] = obj
	s.puts = append(s.puts, key)
	return nil
}

func (s *memoryStore) UpdatedAt(_ context.Context, key string) (time.Time, error) {
	obj, ok := s.objects[key]
	if !ok {
		return time.Time{}, cache.ErrNotFound
	}
	return obj.UpdatedAt, nil
}

func (s *memoryStore) Delete(_ context.Context, key string) error {
	delete(s.objects, key)
	return nil
}

func TestRealMediaWikiUnicodeURLWithFakeBackendAndStore(t *testing.T) {
	const rawURL = "/wiki/%E7%B2%BE%E9%9D%88%E5%AF%B6%E5%8F%AF%E5%A4%A2_Let%27s_Go%EF%BC%81%E7%9A%AE%E5%8D%A1%E4%B8%98%EF%BC%8FLet%27s_Go%EF%BC%81%E4%BC%8A%E5%B8%83"
	const title = "精靈寶可夢_Let's_Go！皮卡丘／Let's_Go！伊布"
	const upstreamPath = "/zh-hant/精靈寶可夢_Let's_Go！皮卡丘／Let's_Go！伊布"
	const escapedUpstreamPath = "/zh-hant/%E7%B2%BE%E9%9D%88%E5%AF%B6%E5%8F%AF%E5%A4%A2_Let%27s_Go%EF%BC%81%E7%9A%AE%E5%8D%A1%E4%B8%98%EF%BC%8FLet%27s_Go%EF%BC%81%E4%BC%8A%E5%B8%83"
	const body = "<html lang=\"zh-hant\">皮卡丘與伊布</html>"

	request := httptest.NewRequest(http.MethodGet, rawURL, nil)
	request.Header.Set("Accept-Language", "zh-TW, zh;q=0.9")
	info := ClassifyRequest(request)
	if !info.Cacheable || info.Title != title || info.Variant != lang.VariantHant {
		t.Fatalf("classification = %#v", info)
	}

	store := newMemoryStore()
	var upstreamURL url.URL
	backend := &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
		upstreamURL = *r.URL
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type":            {"text/html; charset=utf-8"},
				"Content-Language":        {"zh-hant"},
				"Content-Security-Policy": {"default-src 'self'"},
				"Vary":                    {"Cookie"},
			},
			Body: io.NopCloser(strings.NewReader(body)),
		}, nil
	})}
	h := &Handler{
		Cfg: config.Config{
			CacheTTLSeconds: 3600,
			LockTTLSeconds:  45,
		},
		Cache: store,
		MW:    mw.NewClientWithHTTPClient("https://mediawiki.test", backend),
		tryLock: func(context.Context, string, time.Duration) (cacheLock, bool, error) {
			return &fakeCacheLock{}, true, nil
		},
	}
	key := cache.PageKey(info.Variant, info.Title)

	missRecorder := httptest.NewRecorder()
	h.ServeHTTP(missRecorder, request)
	if got := missRecorder.Header().Get("X-Inazuma-Cache"); got != "MISS" {
		t.Fatalf("first cache status = %q, want MISS", got)
	}
	obj, ok := store.objects[key]
	if !ok {
		t.Fatalf("cache key %q was not stored", key)
	}
	if got := upstreamURL.Path; got != upstreamPath {
		t.Fatalf("upstream path = %q, want %q", got, upstreamPath)
	}
	if got := upstreamURL.EscapedPath(); got != escapedUpstreamPath {
		t.Fatalf("escaped upstream path = %q, want %q", got, escapedUpstreamPath)
	}
	if upstreamURL.RawQuery != "" || upstreamURL.Fragment != "" {
		t.Fatalf("title leaked into query or fragment: %#v", upstreamURL)
	}
	if got := string(obj.Body); got != body {
		t.Fatalf("cached body = %q, want %q", got, body)
	}
	if got := missRecorder.Body.String(); got != body {
		t.Fatalf("miss response body = %q, want %q", got, body)
	}
	if len(store.puts) != 1 || store.puts[0] != key {
		t.Fatalf("stored keys = %#v, want [%q]", store.puts, key)
	}

	cachedRequest := httptest.NewRequest(http.MethodGet, rawURL, nil)
	cachedRequest.Header.Set("Accept-Language", "zh-TW")
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, cachedRequest)
	result := recorder.Result()
	if got := recorder.Body.String(); got != body {
		t.Fatalf("served body = %q, want %q", got, body)
	}
	if got := result.Header.Get("X-Inazuma-Cache"); got != "HIT" {
		t.Fatalf("cache status = %q, want HIT", got)
	}
	if got := result.Header.Get("Vary"); got != "Accept-Language" {
		t.Fatalf("Vary = %q, want Accept-Language", got)
	}
	if got := result.Header.Get("Content-Security-Policy"); got != "default-src 'self'" {
		t.Fatalf("Content-Security-Policy = %q", got)
	}
}

func TestMediaWikiSpecialCharacterURLsReachBackendWithoutAliasing(t *testing.T) {
	tests := []struct {
		name           string
		rawURL         string
		acceptLanguage string
		title          string
		variant        string
		upstreamPath   string
		escapedPath    string
	}{
		{
			name:         "literal percent escape remains literal",
			rawURL:       "/wiki/A%252FB_100%2525",
			title:        "A%2FB_100%25",
			variant:      lang.VariantZH,
			upstreamPath: "/zh/A%2FB_100%25",
			escapedPath:  "/zh/A%252FB_100%2525",
		},
		{
			name:         "encoded slash remains a title subpage",
			rawURL:       "/wiki/Anime%2FManga",
			title:        "Anime/Manga",
			variant:      lang.VariantZH,
			upstreamPath: "/zh/Anime/Manga",
			escapedPath:  "/zh/Anime/Manga",
		},
		{
			name:         "dot segments remain part of the title",
			rawURL:       "/index.php?title=Parent%2F..%2FPage",
			title:        "Parent/../Page",
			variant:      lang.VariantZH,
			upstreamPath: "/zh/Parent/../Page",
			escapedPath:  "/zh/Parent/../Page",
		},
		{
			name:         "question mark and hash remain in path",
			rawURL:       "/wiki/Question%3F_%23section",
			title:        "Question?_#section",
			variant:      lang.VariantZH,
			upstreamPath: "/zh/Question?_#section",
			escapedPath:  "/zh/Question%3F_%23section",
		},
		{
			name:           "index.php Unicode title",
			rawURL:         "/index.php?title=Mr._Mime%2F%E9%AD%94%E5%A2%99%E4%BA%BA",
			acceptLanguage: "zh-CN",
			title:          "Mr._Mime/魔墙人",
			variant:        lang.VariantHans,
			upstreamPath:   "/zh-hans/Mr._Mime/魔墙人",
			escapedPath:    "/zh-hans/Mr._Mime/%E9%AD%94%E5%A2%99%E4%BA%BA",
		},
		{
			name:         "accented text and emoji",
			rawURL:       "/wiki/Pok%C3%A9mon_%F0%9F%98%80",
			title:        "Pokémon_😀",
			variant:      lang.VariantZH,
			upstreamPath: "/zh/Pokémon_😀",
			escapedPath:  "/zh/Pok%C3%A9mon_%F0%9F%98%80",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, tt.rawURL, nil)
			request.Header.Set("Accept-Language", tt.acceptLanguage)
			info := ClassifyRequest(request)
			if !info.Cacheable || info.Title != tt.title || info.Variant != tt.variant {
				t.Fatalf("classification = %#v", info)
			}

			store := newMemoryStore()
			var upstreamURL url.URL
			backend := &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
				upstreamURL = *r.URL
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"text/html"}},
					Body:       io.NopCloser(strings.NewReader("page")),
				}, nil
			})}
			h := &Handler{
				Cache: store,
				MW:    mw.NewClientWithHTTPClient("https://mediawiki.test", backend),
			}
			key := cache.PageKey(info.Variant, info.Title)

			if _, nonCacheable, err := h.fetchAndStore(request.Context(), info, key); err != nil {
				t.Fatalf("fetchAndStore: %v", err)
			} else if nonCacheable != nil {
				t.Fatalf("unexpected non-cacheable response: %#v", nonCacheable)
			}
			if got := upstreamURL.Path; got != tt.upstreamPath {
				t.Fatalf("upstream path = %q, want %q", got, tt.upstreamPath)
			}
			if got := upstreamURL.EscapedPath(); got != tt.escapedPath {
				t.Fatalf("escaped path = %q, want %q", got, tt.escapedPath)
			}
			if upstreamURL.RawQuery != "" || upstreamURL.Fragment != "" {
				t.Fatalf("title leaked into query or fragment: %#v", upstreamURL)
			}
			if _, ok := store.objects[key]; !ok {
				t.Fatalf("cache key %q was not stored", key)
			}
		})
	}
}
