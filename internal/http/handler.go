package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/52poke/inazuma/internal/cache"
	"github.com/52poke/inazuma/internal/config"
	"github.com/52poke/inazuma/internal/lock"
	"github.com/52poke/inazuma/internal/mw"
	"github.com/redis/go-redis/v9"
)

type Handler struct {
	Cfg     config.Config
	Cache   cache.Store
	MW      *mw.Client
	Redis   *redis.Client
	Proxy   *httputil.ReverseProxy
	tryLock tryLockFunc
}

type cacheLock interface {
	Unlock(context.Context) error
}

type tryLockFunc func(context.Context, string, time.Duration) (cacheLock, bool, error)

type upstreamResponse struct {
	status int
	header http.Header
	body   []byte
}

const globalRefreshLockKey = "lock:global-refresh"

var cachedResponseHeaderNames = []string{
	"Cache-Control",
	"Content-Language",
	"Content-Security-Policy",
	"Content-Security-Policy-Report-Only",
	"Cross-Origin-Embedder-Policy",
	"Cross-Origin-Opener-Policy",
	"Cross-Origin-Resource-Policy",
	"Permissions-Policy",
	"Referrer-Policy",
	"Strict-Transport-Security",
	"X-Content-Type-Options",
	"X-Frame-Options",
}

func NewHandler(cfg config.Config, store cache.Store, mwClient *mw.Client, redisClient *redis.Client) (*Handler, error) {
	u, err := url.Parse(cfg.MediaWikiBaseURL)
	if err != nil {
		return nil, err
	}
	proxy := httputil.NewSingleHostReverseProxy(u)
	return &Handler{
		Cfg:   cfg,
		Cache: store,
		MW:    mwClient,
		Redis: redisClient,
		Proxy: proxy,
	}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.shouldBypassCache(r) {
		h.Proxy.ServeHTTP(w, r)
		return
	}

	info := ClassifyRequest(r)
	if !info.Cacheable {
		h.Proxy.ServeHTTP(w, r)
		return
	}

	key := cache.PageKey(info.Variant, info.Title)
	obj, err := h.Cache.Get(r.Context(), key)
	if err == nil {
		if !isExpired(obj.UpdatedAt, h.Cfg.CacheTTLSeconds) {
			writeObject(w, obj, "HIT")
			return
		}
		if h.tryRefreshExpired(w, r, key, info) {
			return
		}
		writeObject(w, obj, "STALE")
		return
	}
	if !errors.Is(err, cache.ErrNotFound) {
		h.Proxy.ServeHTTP(w, r)
		return
	}

	obj, ok, upstream := h.getWithLock(r.Context(), key, info)
	if ok {
		writeObject(w, obj, "MISS")
		return
	}

	if upstream != nil {
		writeUpstream(w, upstream)
		return
	}

	// fallback to MediaWiki
	h.Proxy.ServeHTTP(w, r)
}

func (h *Handler) shouldBypassCache(r *http.Request) bool {
	if r.Header.Get("Authorization") != "" {
		return true
	}
	return h.isLoggedIn(r)
}

func (h *Handler) isLoggedIn(r *http.Request) bool {
	name := strings.TrimSpace(h.Cfg.LoggedInCookieName)
	if name == "" {
		return false
	}
	cookie, err := r.Cookie(name)
	if err != nil {
		return false
	}
	return cookie.Value != ""
}

func (h *Handler) getWithLock(ctx context.Context, key string, info RequestInfo) (cache.Object, bool, *upstreamResponse) {
	lockKey := "lock:" + key
	lockTTL := time.Duration(h.Cfg.LockTTLSeconds) * time.Second
	maxWait := time.Duration(h.Cfg.MaxLockWaitSeconds) * time.Second
	deadline := time.Now().Add(maxWait)

	for {
		l, ok, err := h.tryCacheLock(ctx, lockKey, lockTTL)
		if err != nil {
			return cache.Object{}, false, nil
		}
		if ok {
			defer l.Unlock(ctx)
			obj, err := h.Cache.Get(ctx, key)
			if err == nil {
				return obj, true, nil
			}
			obj, upstream, err := h.fetchAndStore(ctx, info, key)
			if err != nil {
				return cache.Object{}, false, nil
			}
			if upstream != nil {
				return cache.Object{}, false, upstream
			}
			return obj, true, nil
		}

		obj, err := h.Cache.Get(ctx, key)
		if err == nil {
			return obj, true, nil
		}

		if time.Now().After(deadline) {
			return cache.Object{}, false, nil
		}
		select {
		case <-ctx.Done():
			return cache.Object{}, false, nil
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (h *Handler) tryRefreshExpired(w http.ResponseWriter, r *http.Request, key string, info RequestInfo) bool {
	lockTTL := time.Duration(h.Cfg.LockTTLSeconds) * time.Second
	globalLock, ok, err := h.tryCacheLock(r.Context(), globalRefreshLockKey, lockTTL)
	if err != nil || !ok {
		return false
	}
	defer globalLock.Unlock(r.Context())

	perKey, ok, err := h.tryCacheLock(r.Context(), "lock:"+key, lockTTL)
	if err != nil || !ok {
		return false
	}
	defer perKey.Unlock(r.Context())

	current, err := h.Cache.Get(r.Context(), key)
	if err == nil && !isExpired(current.UpdatedAt, h.Cfg.CacheTTLSeconds) {
		writeObject(w, current, "HIT")
		return true
	}

	fresh, upstream, err := h.fetchAndStore(r.Context(), info, key)
	if err != nil {
		return false
	}
	if upstream != nil {
		if upstream.status < http.StatusInternalServerError {
			_ = h.Cache.Delete(r.Context(), key)
		}
		writeUpstream(w, upstream)
		return true
	}
	writeObject(w, fresh, "REFRESH")
	return true
}

func (h *Handler) tryCacheLock(ctx context.Context, key string, ttl time.Duration) (cacheLock, bool, error) {
	if h.tryLock != nil {
		return h.tryLock(ctx, key, ttl)
	}
	return lock.TryLock(ctx, h.Redis, key, ttl)
}

func (h *Handler) fetchAndStore(ctx context.Context, info RequestInfo, key string) (cache.Object, *upstreamResponse, error) {
	path := buildVariantPath(info)
	refreshStartedAt := time.Now().UTC()
	resp, body, err := h.MW.Fetch(ctx, path, "", http.Header{})
	if err != nil {
		return cache.Object{}, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return cache.Object{}, &upstreamResponse{
			status: resp.StatusCode,
			header: resp.Header.Clone(),
			body:   body,
		}, nil
	}

	obj := cache.Object{
		Body:        body,
		ContentType: resp.Header.Get("Content-Type"),
		Encoding:    resp.Header.Get("Content-Encoding"),
		Headers:     CacheResponseHeaders(resp.Header),
		UpdatedAt:   refreshStartedAt,
	}
	if err := h.Cache.Put(ctx, key, obj); err != nil {
		return cache.Object{}, nil, err
	}
	return obj, nil, nil
}

func buildVariantPath(info RequestInfo) string {
	switch info.Variant {
	case "zh-hans":
		return "/zh-hans/" + info.Title
	case "zh-hant":
		return "/zh-hant/" + info.Title
	default:
		return "/zh/" + info.Title
	}
}

func writeObject(w http.ResponseWriter, obj cache.Object, cacheStatus string) {
	for _, name := range cachedResponseHeaderNames {
		value := obj.Headers[name]
		if value != "" {
			w.Header().Set(name, value)
		}
	}
	if obj.ContentType != "" {
		w.Header().Set("Content-Type", obj.ContentType)
	}
	if obj.Encoding != "" {
		w.Header().Set("Content-Encoding", obj.Encoding)
	}
	// Variant selection is always based on Accept-Language, regardless of the
	// headers returned by MediaWiki for its explicit variant URL.
	w.Header().Set("Vary", "Accept-Language")
	w.Header().Set("X-Inazuma-Cache", cacheStatus)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(obj.Body)
}

// CacheResponseHeaders returns the end-to-end response metadata that must be
// reproduced when a cached body is served. Vary is deliberately excluded and
// is controlled by Inazuma in writeObject.
func CacheResponseHeaders(headers http.Header) map[string]string {
	cached := make(map[string]string)
	for _, name := range cachedResponseHeaderNames {
		if value := headers.Get(name); value != "" {
			cached[name] = value
		}
	}
	if len(cached) == 0 {
		return nil
	}
	return cached
}

func writeUpstream(w http.ResponseWriter, upstream *upstreamResponse) {
	for k, vv := range upstream.header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(upstream.status)
	_, _ = w.Write(upstream.body)
}

func isExpired(updatedAt time.Time, ttlSeconds int) bool {
	if updatedAt.IsZero() {
		return true
	}
	if ttlSeconds <= 0 {
		return false
	}
	ttl := time.Duration(ttlSeconds) * time.Second
	return updatedAt.Add(ttl).Before(time.Now())
}
