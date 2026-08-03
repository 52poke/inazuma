package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClassifyRequestWhitelist(t *testing.T) {
	tests := []struct {
		name      string
		method    string
		target    string
		cacheable bool
		reason    string
	}{
		{name: "wiki page", method: http.MethodGet, target: "/wiki/Page", cacheable: true},
		{name: "explicit variant", method: http.MethodGet, target: "/zh-hant/Page", cacheable: true},
		{name: "wiki UTM only", method: http.MethodGet, target: "/wiki/Page?utm_source=test", cacheable: true},
		{name: "index title", method: http.MethodGet, target: "/index.php?title=Page", cacheable: true},
		{name: "index title and UTM", method: http.MethodGet, target: "/index.php?title=Page&utm_medium=test", cacheable: true},
		{name: "non-GET", method: http.MethodPost, target: "/wiki/Page", reason: "method-not-get"},
		{name: "wiki extra query", method: http.MethodGet, target: "/wiki/Page?action=edit", reason: "extra-query"},
		{name: "index extra query", method: http.MethodGet, target: "/index.php?title=Page&oldid=1", reason: "extra-query"},
		{name: "index missing title", method: http.MethodGet, target: "/index.php?utm_source=test", reason: "missing-title"},
		{name: "unlisted path", method: http.MethodGet, target: "/api.php?action=query", reason: "extra-query"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := ClassifyRequest(httptest.NewRequest(tt.method, tt.target, nil))
			if info.Cacheable != tt.cacheable || info.Reason != tt.reason {
				t.Fatalf("classification = %#v, want cacheable=%v reason=%q", info, tt.cacheable, tt.reason)
			}
		})
	}
}

func TestClassifyRequestDoesNotDecodePathTitleTwice(t *testing.T) {
	encodedPercent := httptest.NewRequest(http.MethodGet, "/wiki/100%2525", nil)
	percent := httptest.NewRequest(http.MethodGet, "/wiki/100%25", nil)

	encodedInfo := ClassifyRequest(encodedPercent)
	percentInfo := ClassifyRequest(percent)

	if got, want := encodedInfo.Title, "100%25"; got != want {
		t.Fatalf("encoded title = %q, want %q", got, want)
	}
	if got, want := percentInfo.Title, "100%"; got != want {
		t.Fatalf("percent title = %q, want %q", got, want)
	}
	if encodedInfo.Title == percentInfo.Title {
		t.Fatalf("distinct URL titles normalized to the same cache title %q", encodedInfo.Title)
	}
}

func TestClassifyRequestDoesNotDecodeQueryTitleTwice(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/index.php?title=100%2525", nil)

	info := ClassifyRequest(r)

	if got, want := info.Title, "100%25"; got != want {
		t.Fatalf("title = %q, want %q", got, want)
	}
}

func TestNormalizeTitlePreservesPathSyntax(t *testing.T) {
	tests := map[string]string{
		"A/../B": "A/../B",
		"A/./B":  "A/./B",
		"A//B":   "A//B",
	}

	for raw, want := range tests {
		t.Run(raw, func(t *testing.T) {
			if got := NormalizeTitle(raw); got != want {
				t.Fatalf("NormalizeTitle(%q) = %q, want %q", raw, got, want)
			}
		})
	}
}

func TestClassifyRequestRejectsAmbiguousTitleParameters(t *testing.T) {
	tests := []string{
		"/index.php?title=First&title=Second",
		"/index.php?title=First&TITLE=Second",
	}

	for _, target := range tests {
		t.Run(target, func(t *testing.T) {
			info := ClassifyRequest(httptest.NewRequest(http.MethodGet, target, nil))
			if info.Cacheable || info.Reason != "extra-query" {
				t.Fatalf("classification = %#v, want extra-query", info)
			}
		})
	}
}

func TestClassifyRequestRejectsSpecialNamespaceAliases(t *testing.T) {
	tests := []string{
		"/wiki/Special:RecentChanges",
		"/wiki/_Special:RecentChanges",
		"/wiki/%E7%89%B9%E6%AE%8A:RecentChanges",
		"/index.php?title=_%E7%89%B9%E6%AE%8A:RecentChanges",
	}

	for _, target := range tests {
		t.Run(target, func(t *testing.T) {
			info := ClassifyRequest(httptest.NewRequest(http.MethodGet, target, nil))
			if info.Cacheable || info.Reason != "special-page" {
				t.Fatalf("classification = %#v, want special-page", info)
			}
		})
	}
}
