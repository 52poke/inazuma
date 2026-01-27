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
	Cfg   config.Config
	Cache cache.Store
	MW    *mw.Client
	Redis *redis.Client
	Proxy *httputil.ReverseProxy
}

type upstreamResponse struct {
	status int
	header http.Header
	body   []byte
}

const globalRefreshLockKey = "lock:global-refresh"

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
	if h.isLoggedIn(r) {
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
		l, ok, err := lock.TryLock(ctx, h.Redis, lockKey, lockTTL)
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
	globalLock, ok, err := lock.TryLock(r.Context(), h.Redis, globalRefreshLockKey, lockTTL)
	if err != nil || !ok {
		return false
	}
	defer globalLock.Unlock(r.Context())

	perKey, ok, err := lock.TryLock(r.Context(), h.Redis, "lock:"+key, lockTTL)
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

func (h *Handler) fetchAndStore(ctx context.Context, info RequestInfo, key string) (cache.Object, *upstreamResponse, error) {
	path := buildVariantPath(info)
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
		UpdatedAt:   time.Now().UTC(),
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
	if obj.ContentType != "" {
		w.Header().Set("Content-Type", obj.ContentType)
	}
	if obj.Encoding != "" {
		w.Header().Set("Content-Encoding", obj.Encoding)
	}
	w.Header().Set("X-Inazuma-Cache", cacheStatus)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(obj.Body)
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
