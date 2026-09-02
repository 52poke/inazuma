package httpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/52poke/inazuma/internal/cache"
	"github.com/52poke/inazuma/internal/config"
	"github.com/52poke/inazuma/internal/lock"
	"github.com/52poke/inazuma/internal/mw"
)

type configurableStore struct {
	get func(context.Context, string) (cache.Object, error)
	put func(context.Context, string, cache.Object) error
}

func (s *configurableStore) Get(ctx context.Context, key string) (cache.Object, error) {
	return s.get(ctx, key)
}

func (s *configurableStore) Put(ctx context.Context, key string, obj cache.Object) error {
	if s.put == nil {
		return nil
	}
	return s.put(ctx, key, obj)
}

func (*configurableStore) UpdatedAt(context.Context, string) (time.Time, error) {
	return time.Time{}, cache.ErrNotFound
}

func (*configurableStore) Delete(context.Context, string) error { return nil }

type testCacheLock struct{}

func (*testCacheLock) Unlock(context.Context) error { return nil }

type notifyingCacheLock struct {
	unlocked chan<- struct{}
}

func (l *notifyingCacheLock) Unlock(context.Context) error {
	select {
	case l.unlocked <- struct{}{}:
	default:
	}
	return nil
}

func originClient(status int, body string, calls *int) *mw.Client {
	client := &http.Client{Transport: fakeRoundTripper(func(*http.Request) (*http.Response, error) {
		(*calls)++
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": {"text/html; charset=utf-8"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}
	return mw.NewClientWithHTTPClient("https://mediawiki.test", client)
}

func cacheableRequest() *http.Request {
	return httptest.NewRequest(http.MethodGet, "/wiki/Pikachu", nil)
}

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

func TestCacheReadFailureReturnsServiceUnavailableWithoutOriginRequest(t *testing.T) {
	originCalls := 0
	h := &Handler{
		Cfg: config.Config{MaxLockWaitSeconds: 3},
		Cache: &configurableStore{get: func(context.Context, string) (cache.Object, error) {
			return cache.Object{}, errors.New("S3 unavailable")
		}},
		MW: originClient(http.StatusOK, "origin", &originCalls),
	}

	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, cacheableRequest())

	if got := recorder.Code; got != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", got)
	}
	if got := recorder.Header().Get("Retry-After"); got != "3" {
		t.Fatalf("Retry-After = %q, want 3", got)
	}
	if originCalls != 0 {
		t.Fatalf("origin calls = %d, want 0", originCalls)
	}
}

func TestRedisFailureOnColdMissReturnsServiceUnavailable(t *testing.T) {
	originCalls := 0
	h := &Handler{
		Cfg: config.Config{LockTTLSeconds: 60},
		Cache: &configurableStore{get: func(context.Context, string) (cache.Object, error) {
			return cache.Object{}, cache.ErrNotFound
		}},
		MW: originClient(http.StatusOK, "origin", &originCalls),
		tryLock: func(context.Context, string, time.Duration) (cacheLock, bool, error) {
			return nil, false, errors.New("Redis unavailable")
		},
	}

	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, cacheableRequest())

	if got := recorder.Code; got != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", got)
	}
	if got := recorder.Header().Get("Retry-After"); got == "" {
		t.Fatal("Retry-After is missing")
	}
	if originCalls != 0 {
		t.Fatalf("origin calls = %d, want 0", originCalls)
	}
}

func TestColdMissLockWaitExpiryReturnsServiceUnavailable(t *testing.T) {
	originCalls := 0
	h := &Handler{
		Cfg: config.Config{LockTTLSeconds: 60, MaxLockWaitSeconds: 0},
		Cache: &configurableStore{get: func(context.Context, string) (cache.Object, error) {
			return cache.Object{}, cache.ErrNotFound
		}},
		MW: originClient(http.StatusOK, "origin", &originCalls),
		tryLock: func(context.Context, string, time.Duration) (cacheLock, bool, error) {
			return nil, false, nil
		},
	}

	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, cacheableRequest())

	if got := recorder.Code; got != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", got)
	}
	if originCalls != 0 {
		t.Fatalf("origin calls = %d, want 0", originCalls)
	}
}

func TestColdMissRequiresOriginCapacity(t *testing.T) {
	originCalls := 0
	var lockKeys []string
	h := &Handler{
		Cfg: config.Config{LockTTLSeconds: 60, MaxLockWaitSeconds: 0, MaxConcurrentRenders: 2},
		Cache: &configurableStore{get: func(context.Context, string) (cache.Object, error) {
			return cache.Object{}, cache.ErrNotFound
		}},
		MW: originClient(http.StatusOK, "origin", &originCalls),
		tryLock: func(_ context.Context, key string, _ time.Duration) (cacheLock, bool, error) {
			lockKeys = append(lockKeys, key)
			if key == lock.RenderCapacityLockKey(0) || key == lock.RenderCapacityLockKey(1) {
				return nil, false, nil
			}
			return &testCacheLock{}, true, nil
		},
	}

	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, cacheableRequest())

	if got := recorder.Code; got != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", got)
	}
	if originCalls != 0 {
		t.Fatalf("origin calls = %d, want 0", originCalls)
	}
	if len(lockKeys) != 3 || lockKeys[1] != lock.RenderCapacityLockKey(0) || lockKeys[2] != lock.RenderCapacityLockKey(1) {
		t.Fatalf("lock keys = %#v, want per-key lock followed by both capacity slots", lockKeys)
	}
}

func TestColdMissUsesNextAvailableOriginCapacitySlot(t *testing.T) {
	originCalls := 0
	var lockKeys []string
	h := &Handler{
		Cfg: config.Config{LockTTLSeconds: 60, MaxConcurrentRenders: 2},
		Cache: &configurableStore{get: func(context.Context, string) (cache.Object, error) {
			return cache.Object{}, cache.ErrNotFound
		}},
		MW: originClient(http.StatusOK, "origin", &originCalls),
		tryLock: func(_ context.Context, key string, _ time.Duration) (cacheLock, bool, error) {
			lockKeys = append(lockKeys, key)
			if key == lock.RenderCapacityLockKey(0) {
				return nil, false, nil
			}
			return &testCacheLock{}, true, nil
		},
	}

	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, cacheableRequest())

	if got := recorder.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}
	if originCalls != 1 {
		t.Fatalf("origin calls = %d, want 1", originCalls)
	}
	if len(lockKeys) != 3 || lockKeys[1] != lock.RenderCapacityLockKey(0) || lockKeys[2] != lock.RenderCapacityLockKey(1) {
		t.Fatalf("lock keys = %#v, want the second capacity slot after the first is occupied", lockKeys)
	}
}

func TestSuccessfulRenderIsServedWhenCacheWriteFails(t *testing.T) {
	originCalls := 0
	putCalls := 0
	h := &Handler{
		Cfg: config.Config{LockTTLSeconds: 60, MaxConcurrentRenders: 2},
		Cache: &configurableStore{
			get: func(context.Context, string) (cache.Object, error) {
				return cache.Object{}, cache.ErrNotFound
			},
			put: func(context.Context, string, cache.Object) error {
				putCalls++
				return errors.New("S3 write failed")
			},
		},
		MW: originClient(http.StatusOK, "already rendered", &originCalls),
		tryLock: func(context.Context, string, time.Duration) (cacheLock, bool, error) {
			return &testCacheLock{}, true, nil
		},
	}

	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, cacheableRequest())

	if got := recorder.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}
	if got := recorder.Body.String(); got != "already rendered" {
		t.Fatalf("body = %q", got)
	}
	if got := recorder.Header().Get("X-Inazuma-Cache"); got != "MISS" {
		t.Fatalf("cache status = %q, want MISS", got)
	}
	if originCalls != 1 || putCalls != 1 {
		t.Fatalf("origin calls = %d, cache puts = %d; want one each", originCalls, putCalls)
	}
}

func TestStaleObjectIsServedWhenRedisIsUnavailable(t *testing.T) {
	originCalls := 0
	lockAttempted := make(chan struct{}, 1)
	stale := cache.Object{Body: []byte("stale"), UpdatedAt: time.Now().Add(-time.Hour)}
	h := &Handler{
		Cfg: config.Config{CacheTTLSeconds: 1, LockTTLSeconds: 60, MaxConcurrentRenders: 2},
		Cache: &configurableStore{get: func(context.Context, string) (cache.Object, error) {
			return stale, nil
		}},
		MW: originClient(http.StatusOK, "origin", &originCalls),
		tryLock: func(context.Context, string, time.Duration) (cacheLock, bool, error) {
			lockAttempted <- struct{}{}
			return nil, false, errors.New("Redis unavailable")
		},
	}

	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, cacheableRequest())

	if got := recorder.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}
	if got := recorder.Body.String(); got != "stale" {
		t.Fatalf("body = %q, want stale", got)
	}
	if got := recorder.Header().Get("X-Inazuma-Cache"); got != "STALE" {
		t.Fatalf("cache status = %q, want STALE", got)
	}
	select {
	case <-lockAttempted:
	case <-time.After(time.Second):
		t.Fatal("background refresh did not attempt the Redis lock")
	}
	if originCalls != 0 {
		t.Fatalf("origin calls = %d, want 0", originCalls)
	}
}

func TestStaleObjectIsServedWhenBoundedRefreshFails(t *testing.T) {
	originCalls := 0
	refreshDone := make(chan struct{}, 2)
	stale := cache.Object{Body: []byte("stale"), UpdatedAt: time.Now().Add(-time.Hour)}
	client := &http.Client{Transport: fakeRoundTripper(func(*http.Request) (*http.Response, error) {
		originCalls++
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("origin failure")),
		}, nil
	})}
	h := &Handler{
		Cfg: config.Config{CacheTTLSeconds: 1, LockTTLSeconds: 60, MaxConcurrentRenders: 2},
		Cache: &configurableStore{get: func(context.Context, string) (cache.Object, error) {
			return stale, nil
		}},
		MW: mw.NewClientWithHTTPClient("https://mediawiki.test", client),
		tryLock: func(context.Context, string, time.Duration) (cacheLock, bool, error) {
			return &notifyingCacheLock{unlocked: refreshDone}, true, nil
		},
	}

	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, cacheableRequest())

	if got := recorder.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}
	if got := recorder.Body.String(); got != "stale" {
		t.Fatalf("body = %q, want stale", got)
	}
	if got := recorder.Header().Get("X-Inazuma-Cache"); got != "STALE" {
		t.Fatalf("cache status = %q, want STALE", got)
	}
	select {
	case <-refreshDone:
	case <-time.After(time.Second):
		t.Fatal("background refresh did not finish")
	}
	if originCalls != 1 {
		t.Fatalf("origin calls = %d, want one bounded refresh", originCalls)
	}
}

func TestStaleObjectDoesNotWaitForBackgroundRefresh(t *testing.T) {
	originStarted := make(chan struct{})
	releaseOrigin := make(chan struct{})
	refreshDone := make(chan struct{}, 2)
	stale := cache.Object{Body: []byte("stale"), UpdatedAt: time.Now().Add(-time.Hour)}
	client := &http.Client{Transport: fakeRoundTripper(func(*http.Request) (*http.Response, error) {
		close(originStarted)
		<-releaseOrigin
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/html; charset=utf-8"}},
			Body:       io.NopCloser(strings.NewReader("fresh")),
		}, nil
	})}
	h := &Handler{
		Cfg: config.Config{CacheTTLSeconds: 1, LockTTLSeconds: 60, MaxConcurrentRenders: 2},
		Cache: &configurableStore{get: func(context.Context, string) (cache.Object, error) {
			return stale, nil
		}},
		MW: mw.NewClientWithHTTPClient("https://mediawiki.test", client),
		tryLock: func(context.Context, string, time.Duration) (cacheLock, bool, error) {
			return &notifyingCacheLock{unlocked: refreshDone}, true, nil
		},
	}

	recorder := httptest.NewRecorder()
	responseDone := make(chan struct{})
	go func() {
		h.ServeHTTP(recorder, cacheableRequest())
		close(responseDone)
	}()

	select {
	case <-responseDone:
	case <-time.After(time.Second):
		close(releaseOrigin)
		t.Fatal("stale response waited for the origin refresh")
	}
	if got := recorder.Body.String(); got != "stale" {
		t.Fatalf("body = %q, want stale", got)
	}
	if got := recorder.Header().Get("X-Inazuma-Cache"); got != "STALE" {
		t.Fatalf("cache status = %q, want STALE", got)
	}

	select {
	case <-originStarted:
	case <-time.After(time.Second):
		t.Fatal("background refresh did not reach the origin")
	}
	close(releaseOrigin)
	select {
	case <-refreshDone:
	case <-time.After(time.Second):
		t.Fatal("background refresh did not finish")
	}
}
