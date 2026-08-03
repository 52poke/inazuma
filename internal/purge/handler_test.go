package purge

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/52poke/inazuma/internal/cache"
	"github.com/52poke/inazuma/internal/lang"
	"github.com/52poke/inazuma/internal/mw"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type fakeStore struct {
	updatedAt time.Time
	deleted   string
	object    cache.Object
}

func (s *fakeStore) Get(context.Context, string) (cache.Object, error) {
	if s.object.Body == nil {
		return cache.Object{}, cache.ErrNotFound
	}
	return s.object, nil
}

func (s *fakeStore) Put(_ context.Context, _ string, obj cache.Object) error {
	s.object = obj
	s.updatedAt = obj.UpdatedAt
	return nil
}

func (s *fakeStore) UpdatedAt(context.Context, string) (time.Time, error) {
	if s.updatedAt.IsZero() {
		return time.Time{}, cache.ErrNotFound
	}
	return s.updatedAt, nil
}

func (s *fakeStore) Delete(_ context.Context, key string) error {
	s.deleted = key
	s.object = cache.Object{}
	return nil
}

func TestRefreshVariantRetriesNginxPurgeForNewerObject(t *testing.T) {
	store := &fakeStore{updatedAt: time.Now().UTC()}
	var method, path string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		method, path = r.Method, r.URL.Path
		return response(http.StatusNoContent, ""), nil
	})}

	h := &Handler{Cache: store, NginxPurge: "http://nginx.test", HTTPClient: client}
	err := h.refreshVariant(context.Background(), "Page", lang.VariantZH, store.updatedAt.Add(-time.Minute))
	if err != nil {
		t.Fatalf("refreshVariant: %v", err)
	}
	if method != "PURGE" || path != "/zh/Page" {
		t.Fatalf("nginx request = %s %s, want PURGE /zh/Page", method, path)
	}
}

func TestRefreshLockedPurgesNginxAfterDeletingNon200(t *testing.T) {
	upstreamClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusNotFound, "missing"), nil
	})}

	nginxPurged := false
	nginxClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		nginxPurged = r.Method == "PURGE" && r.URL.Path == "/zh/Missing"
		return response(http.StatusNoContent, ""), nil
	})}

	store := &fakeStore{object: cache.Object{Body: []byte("stale")}}
	h := &Handler{
		Cache:      store,
		MW:         mw.NewClientWithHTTPClient("http://mediawiki.test", upstreamClient),
		NginxPurge: "http://nginx.test",
		HTTPClient: nginxClient,
	}
	key := cache.PageKey(lang.VariantZH, "Missing")

	if err := h.refreshLocked(context.Background(), key, "/zh/Missing"); err != nil {
		t.Fatalf("refreshLocked: %v", err)
	}
	if store.deleted != key {
		t.Fatalf("deleted key = %q, want %q", store.deleted, key)
	}
	if !nginxPurged {
		t.Fatal("Nginx was not purged after deleting the stale object")
	}
}

func TestNginxHTTPClientHasDefaultTimeout(t *testing.T) {
	h := &Handler{}
	if got := h.nginxHTTPClient().Timeout; got != defaultNginxPurgeTimeout {
		t.Fatalf("default timeout = %s, want %s", got, defaultNginxPurgeTimeout)
	}
}

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
