package httpx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
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
	Logger  *slog.Logger
	tryLock tryLockFunc

	refreshMu           sync.Mutex
	backgroundRefreshes map[string]struct{}
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
		writeObject(w, obj, "STALE")
		h.refreshExpiredAsync(key, info)
		return
	}
	if !errors.Is(err, cache.ErrNotFound) {
		h.logger().Error("cache read failed", "path", r.URL.Path, "error", err)
		h.writeServiceUnavailable(w)
		return
	}

	obj, ok, upstream, err := h.getWithLock(r.Context(), key, info)
	if err != nil {
		h.logger().Error("request processing failed", "path", r.URL.Path, "error", err)
		h.writeServiceUnavailable(w)
		return
	}
	if ok {
		writeObject(w, obj, "MISS")
		return
	}

	if upstream != nil {
		writeUpstream(w, upstream)
		return
	}

	// Cacheable anonymous traffic must never bypass the origin-capacity locks.
	h.writeServiceUnavailable(w)
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

func (h *Handler) getWithLock(ctx context.Context, key string, info RequestInfo) (cache.Object, bool, *upstreamResponse, error) {
	lockKey := "lock:" + key
	lockTTL := time.Duration(h.Cfg.LockTTLSeconds) * time.Second
	maxWait := time.Duration(h.Cfg.MaxLockWaitSeconds) * time.Second
	deadline := time.Now().Add(maxWait)

	for {
		l, ok, err := h.tryCacheLock(ctx, lockKey, lockTTL)
		if err != nil {
			return cache.Object{}, false, nil, fmt.Errorf("acquire page lock: %w", err)
		}
		if ok {
			return h.renderColdMiss(ctx, l, key, info, lockTTL, deadline)
		}

		obj, err := h.Cache.Get(ctx, key)
		if err == nil {
			return obj, true, nil, nil
		}
		if !errors.Is(err, cache.ErrNotFound) {
			return cache.Object{}, false, nil, fmt.Errorf("read cache while waiting for page lock: %w", err)
		}

		if !waitForLockRetry(ctx, deadline) {
			if err := ctx.Err(); err != nil {
				return cache.Object{}, false, nil, err
			}
			return cache.Object{}, false, nil, errors.New("timed out waiting for page lock")
		}
	}
}

// renderColdMiss is entered with the per-key lock held. The origin-capacity
// slots prevent unrelated cold misses from rendering at MediaWiki without a
// bound even though each one holds a different per-key lock.
func (h *Handler) renderColdMiss(ctx context.Context, perKey cacheLock, key string, info RequestInfo, lockTTL time.Duration, deadline time.Time) (cache.Object, bool, *upstreamResponse, error) {
	defer perKey.Unlock(ctx)

	obj, err := h.Cache.Get(ctx, key)
	if err == nil {
		return obj, true, nil, nil
	}
	if !errors.Is(err, cache.ErrNotFound) {
		return cache.Object{}, false, nil, fmt.Errorf("read cache after acquiring page lock: %w", err)
	}

	for {
		capacityLock, ok, err := h.tryOriginCapacityLock(ctx, lockTTL)
		if err != nil {
			return cache.Object{}, false, nil, fmt.Errorf("acquire render capacity: %w", err)
		}
		if ok {
			defer capacityLock.Unlock(ctx)
			obj, upstream, err := h.fetchAndStore(ctx, info, key)
			if err != nil {
				return cache.Object{}, false, nil, fmt.Errorf("fetch origin: %w", err)
			}
			if upstream != nil {
				return cache.Object{}, false, upstream, nil
			}
			return obj, true, nil, nil
		}
		if !waitForLockRetry(ctx, deadline) {
			if err := ctx.Err(); err != nil {
				return cache.Object{}, false, nil, err
			}
			return cache.Object{}, false, nil, errors.New("timed out waiting for render capacity")
		}
	}
}

func waitForLockRetry(ctx context.Context, deadline time.Time) bool {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false
	}
	delay := 50 * time.Millisecond
	if remaining < delay {
		delay = remaining
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (h *Handler) refreshExpiredAsync(key string, info RequestInfo) {
	if !h.beginBackgroundRefresh(key) {
		return
	}
	lockTTL := time.Duration(h.Cfg.LockTTLSeconds) * time.Second
	go func() {
		defer h.finishBackgroundRefresh(key)

		ctx, cancel := context.WithTimeout(context.Background(), lockTTL)
		defer cancel()

		perKey, ok, err := h.tryCacheLock(ctx, "lock:"+key, lockTTL)
		if err != nil {
			h.logger().Error("background refresh lock failed", "key", key, "error", err)
			return
		}
		if !ok {
			return
		}
		defer unlockInBackground(perKey)

		current, err := h.Cache.Get(ctx, key)
		if err != nil {
			if !errors.Is(err, cache.ErrNotFound) {
				h.logger().Error("background refresh cache read failed", "key", key, "error", err)
			}
			return
		}
		if !isExpired(current.UpdatedAt, h.Cfg.CacheTTLSeconds) {
			return
		}

		capacityLock, ok, err := h.tryOriginCapacityLock(ctx, lockTTL)
		if err != nil {
			h.logger().Error("background refresh capacity lock failed", "key", key, "error", err)
			return
		}
		if !ok {
			return
		}
		defer unlockInBackground(capacityLock)

		_, upstream, err := h.fetchAndStore(ctx, info, key)
		if err != nil {
			h.logger().Error("background refresh failed", "key", key, "error", err)
			return
		}
		if upstream == nil {
			return
		}
		if upstream.status < http.StatusInternalServerError {
			if err := h.Cache.Delete(ctx, key); err != nil {
				h.logger().Error("background refresh cache delete failed", "key", key, "error", err)
			}
			return
		}
		h.logger().Error("background refresh origin error", "key", key, "status", upstream.status)
	}()
}

func (h *Handler) beginBackgroundRefresh(key string) bool {
	h.refreshMu.Lock()
	defer h.refreshMu.Unlock()
	if h.backgroundRefreshes == nil {
		h.backgroundRefreshes = make(map[string]struct{})
	}
	if _, exists := h.backgroundRefreshes[key]; exists {
		return false
	}
	h.backgroundRefreshes[key] = struct{}{}
	return true
}

func (h *Handler) finishBackgroundRefresh(key string) {
	h.refreshMu.Lock()
	delete(h.backgroundRefreshes, key)
	h.refreshMu.Unlock()
}

func unlockInBackground(l cacheLock) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = l.Unlock(ctx)
}

func (h *Handler) tryCacheLock(ctx context.Context, key string, ttl time.Duration) (cacheLock, bool, error) {
	if h.tryLock != nil {
		return h.tryLock(ctx, key, ttl)
	}
	return lock.TryLock(ctx, h.Redis, key, ttl)
}

func (h *Handler) tryOriginCapacityLock(ctx context.Context, ttl time.Duration) (cacheLock, bool, error) {
	for slot := 0; slot < h.Cfg.MaxConcurrentRenders; slot++ {
		key := lock.RenderCapacityLockKey(slot)
		capacityLock, ok, err := h.tryCacheLock(ctx, key, ttl)
		if err != nil {
			return nil, false, err
		}
		if ok {
			return capacityLock, true, nil
		}
	}
	return nil, false, nil
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
		// The origin work has already completed. Serving this in-memory response
		// avoids a second render for the same request when object storage is down.
		h.logger().Error("cache write failed", "key", key, "error", err)
		return obj, nil, nil
	}
	return obj, nil, nil
}

func (h *Handler) logger() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

func (h *Handler) writeServiceUnavailable(w http.ResponseWriter) {
	retryAfter := h.Cfg.MaxLockWaitSeconds
	if retryAfter < 1 {
		retryAfter = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
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
