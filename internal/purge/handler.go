package purge

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/52poke/inazuma/internal/cache"
	httpx "github.com/52poke/inazuma/internal/http"
	"github.com/52poke/inazuma/internal/lang"
	"github.com/52poke/inazuma/internal/lock"
	"github.com/52poke/inazuma/internal/mw"
	"github.com/redis/go-redis/v9"
)

type Handler struct {
	Cache      cache.Store
	MW         *mw.Client
	Redis      *redis.Client
	NginxPurge string
	LockTTL    time.Duration
	LockWait   time.Duration
	HTTPClient *http.Client
}

const purgeTimestampHeader = "X-Purge-Timestamp"
const defaultNginxPurgeTimeout = 10 * time.Second

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	title, variants, err := parsePath(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	title = httpx.NormalizeTitle(title)
	if title == "" {
		http.Error(w, "title required", http.StatusBadRequest)
		return
	}

	tsHeader := strings.TrimSpace(r.Header.Get(purgeTimestampHeader))
	if tsHeader == "" {
		http.Error(w, "missing purge timestamp", http.StatusBadRequest)
		return
	}
	purgeTime, err := time.Parse(time.RFC3339, tsHeader)
	if err != nil {
		http.Error(w, "invalid purge timestamp", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	for _, variant := range variants {
		if err := h.refreshVariant(ctx, title, variant, purgeTime); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

func parsePath(path string) (string, []string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil, errors.New("path required")
	}
	switch {
	case strings.HasPrefix(path, "/wiki/"):
		title := strings.TrimPrefix(path, "/wiki/")
		if strings.TrimSpace(title) == "" {
			return "", nil, errors.New("title required")
		}
		return title, []string{lang.VariantZH, lang.VariantHans, lang.VariantHant}, nil
	case strings.HasPrefix(path, "/zh-hans/"):
		title := strings.TrimPrefix(path, "/zh-hans/")
		if strings.TrimSpace(title) == "" {
			return "", nil, errors.New("title required")
		}
		return title, []string{lang.VariantHans}, nil
	case strings.HasPrefix(path, "/zh-hant/"):
		title := strings.TrimPrefix(path, "/zh-hant/")
		if strings.TrimSpace(title) == "" {
			return "", nil, errors.New("title required")
		}
		return title, []string{lang.VariantHant}, nil
	case strings.HasPrefix(path, "/zh/"):
		title := strings.TrimPrefix(path, "/zh/")
		if strings.TrimSpace(title) == "" {
			return "", nil, errors.New("title required")
		}
		return title, []string{lang.VariantZH}, nil
	default:
		return "", nil, errors.New("unsupported purge path")
	}
}

func (h *Handler) refreshVariant(ctx context.Context, title, variant string, purgeTime time.Time) error {
	key := cache.PageKey(variant, title)
	path := variantPath(variant, title)
	updatedAt, err := h.Cache.UpdatedAt(ctx, key)
	if err == nil && updatedAt.After(purgeTime) {
		// The object may have been stored before an earlier Nginx purge attempt
		// failed. Always retry the outer-cache invalidation.
		return h.purgeNginx(ctx, path)
	}
	if err != nil && !errors.Is(err, cache.ErrNotFound) {
		return err
	}

	lockKey := "lock:" + key
	l, err := waitForLock(ctx, h.Redis, lockKey, h.LockTTL, h.LockWait)
	if err != nil {
		return err
	}
	defer l.Unlock(ctx)

	// Do not skip based on UpdatedAt after waiting for the lock. A previous holder
	// may have started its fetch before the purge event.
	return h.refreshLocked(ctx, key, path)
}

func (h *Handler) refreshLocked(ctx context.Context, key, path string) error {
	refreshStartedAt := time.Now().UTC()
	resp, body, err := h.MW.Fetch(ctx, path, "", http.Header{})
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode < http.StatusInternalServerError {
			if err := h.Cache.Delete(ctx, key); err != nil {
				return err
			}
			return h.purgeNginx(ctx, path)
		}
		return errors.New("upstream non-200 response")
	}

	obj := cache.Object{
		Body:        body,
		ContentType: resp.Header.Get("Content-Type"),
		Encoding:    resp.Header.Get("Content-Encoding"),
		Headers:     httpx.CacheResponseHeaders(resp.Header),
		UpdatedAt:   refreshStartedAt,
	}
	if err := h.Cache.Put(ctx, key, obj); err != nil {
		return err
	}

	return h.purgeNginx(ctx, path)
}

func waitForLock(ctx context.Context, client *redis.Client, key string, ttl, maxWait time.Duration) (*lock.RedisLock, error) {
	deadline := time.Now().Add(maxWait)
	for {
		l, ok, err := lock.TryLock(ctx, client, key, ttl)
		if err != nil {
			return nil, err
		}
		if ok {
			return l, nil
		}
		if maxWait <= 0 || !time.Now().Before(deadline) {
			return nil, errors.New("timed out waiting for cache refresh lock")
		}

		wait := 50 * time.Millisecond
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining
		}
		timer := time.NewTimer(wait)

		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func variantPath(variant, title string) string {
	switch variant {
	case lang.VariantHans:
		return "/zh-hans/" + title
	case lang.VariantHant:
		return "/zh-hant/" + title
	default:
		return "/zh/" + title
	}
}

func (h *Handler) purgeNginx(ctx context.Context, path string) error {
	if strings.TrimSpace(h.NginxPurge) == "" {
		return nil
	}
	base, err := url.Parse(h.NginxPurge)
	if err != nil {
		return err
	}
	base.Path = strings.TrimRight(base.Path, "/") + path
	req, err := http.NewRequestWithContext(ctx, "PURGE", base.String(), nil)
	if err != nil {
		return err
	}
	client := h.nginxHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return errors.New("nginx purge failed")
	}
	return nil
}

func (h *Handler) nginxHTTPClient() *http.Client {
	if h.HTTPClient != nil {
		return h.HTTPClient
	}
	return &http.Client{Timeout: defaultNginxPurgeTimeout}
}
