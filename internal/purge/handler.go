package purge

import (
	"context"
	"encoding/json"
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
	HTTPClient *http.Client
}

const purgeTimestampHeader = "X-Purge-Timestamp"

type purgePayload struct {
	Title string `json:"title"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	title, err := readTitle(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	title = strings.TrimSpace(title)
	if title == "" {
		http.Error(w, "title required", http.StatusBadRequest)
		return
	}
	title = httpx.NormalizeTitle(title)

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
	variants := []string{lang.VariantZH, lang.VariantHans, lang.VariantHant}
	for _, variant := range variants {
		if err := h.refreshVariant(ctx, title, variant, purgeTime); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

func readTitle(r *http.Request) (string, error) {
	if v := r.URL.Query().Get("title"); v != "" {
		return v, nil
	}
	if v := r.Header.Get("X-Title"); v != "" {
		return v, nil
	}
	if r.Body == nil {
		return "", errors.New("title not found")
	}
	defer r.Body.Close()
	var payload purgePayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err == nil && payload.Title != "" {
		return payload.Title, nil
	}
	return "", errors.New("title not found")
}

func (h *Handler) refreshVariant(ctx context.Context, title, variant string, purgeTime time.Time) error {
	key := cache.PageKey(variant, title)
	updatedAt, err := h.Cache.UpdatedAt(ctx, key)
	if err == nil && updatedAt.After(purgeTime) {
		return nil
	}
	if err != nil && !errors.Is(err, cache.ErrNotFound) {
		return err
	}

	lockKey := "lock:" + key
	l, ok, err := lock.TryLock(ctx, h.Redis, lockKey, h.LockTTL)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	defer l.Unlock(ctx)

	updatedAt, err = h.Cache.UpdatedAt(ctx, key)
	if err == nil && updatedAt.After(purgeTime) {
		return nil
	}
	if err != nil && !errors.Is(err, cache.ErrNotFound) {
		return err
	}

	path := variantPath(variant, title)
	resp, body, err := h.MW.Fetch(ctx, path, "", http.Header{})
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode < http.StatusInternalServerError {
			_ = h.Cache.Delete(ctx, key)
			return nil
		}
		return errors.New("upstream non-200 response")
	}

	obj := cache.Object{
		Body:        body,
		ContentType: resp.Header.Get("Content-Type"),
		Encoding:    resp.Header.Get("Content-Encoding"),
		UpdatedAt:   time.Now().UTC(),
	}
	if err := h.Cache.Put(ctx, key, obj); err != nil {
		return err
	}

	return h.purgeNginx(ctx, path)
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
	client := h.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
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
