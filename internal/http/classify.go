package httpx

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/52poke/inazuma/internal/lang"
)

type RequestInfo struct {
	Cacheable bool
	Title     string
	Variant   string
	Reason    string
}

func ClassifyRequest(r *http.Request) RequestInfo {
	if r.Method != http.MethodGet {
		return RequestInfo{Cacheable: false, Reason: "method-not-get"}
	}

	cleaned, nonUTM := stripUTMParams(r.URL)
	if nonUTM {
		return RequestInfo{Cacheable: false, Reason: "extra-query"}
	}

	switch {
	case strings.HasPrefix(cleaned.Path, "/wiki/"):
		title := strings.TrimPrefix(cleaned.Path, "/wiki/")
		return buildCacheable(title, lang.VariantFromAcceptLanguage(r.Header.Get("Accept-Language")))
	case strings.HasPrefix(cleaned.Path, "/zh-hans/"):
		title := strings.TrimPrefix(cleaned.Path, "/zh-hans/")
		return buildCacheable(title, lang.VariantHans)
	case strings.HasPrefix(cleaned.Path, "/zh-hant/"):
		title := strings.TrimPrefix(cleaned.Path, "/zh-hant/")
		return buildCacheable(title, lang.VariantHant)
	case strings.HasPrefix(cleaned.Path, "/zh/"):
		title := strings.TrimPrefix(cleaned.Path, "/zh/")
		return buildCacheable(title, lang.VariantZH)
	case cleaned.Path == "/index.php":
		title := cleaned.Query().Get("title")
		if title == "" {
			return RequestInfo{Cacheable: false, Reason: "missing-title"}
		}
		return buildCacheable(title, lang.VariantFromAcceptLanguage(r.Header.Get("Accept-Language")))
	default:
		return RequestInfo{Cacheable: false, Reason: "not-page"}
	}
}

func buildCacheable(rawTitle string, variant string) RequestInfo {
	title := NormalizeTitle(rawTitle)
	if title == "" {
		return RequestInfo{Cacheable: false, Reason: "empty-title"}
	}
	if isSpecialPage(title) {
		return RequestInfo{Cacheable: false, Reason: "special-page"}
	}
	return RequestInfo{Cacheable: true, Title: title, Variant: variant}
}

func NormalizeTitle(raw string) string {
	// URL.Path and url.Values have already been unescaped by net/url. Unescaping
	// here again aliases distinct titles such as "%25" and "%2525".
	// Titles are not filesystem paths, so dot segments and repeated slashes must
	// also be preserved rather than normalized with path.Clean.
	return strings.ReplaceAll(raw, " ", "_")
}

func isSpecialPage(title string) bool {
	// MediaWiki treats spaces and underscores at the start of a title as
	// insignificant. 52Poké also configures 特殊 as an alias for the canonical
	// Special namespace.
	title = strings.TrimLeft(title, " _\t\r\n")
	lower := strings.ToLower(title)
	return strings.HasPrefix(lower, "special:") || strings.HasPrefix(lower, "特殊:")
}

func stripUTMParams(u *url.URL) (*url.URL, bool) {
	clone := *u
	q := clone.Query()
	for key := range q {
		if strings.HasPrefix(strings.ToLower(key), "utm_") {
			q.Del(key)
		}
	}
	clone.RawQuery = q.Encode()
	if len(q) == 0 {
		return &clone, false
	}

	if clone.Path == "/index.php" {
		// Query parameter names are case-sensitive, and duplicate title values are
		// ambiguous. Only one exact title parameter is safe to canonicalize.
		if titles, ok := q["title"]; ok && len(q) == 1 && len(titles) == 1 {
			return &clone, false
		}
	}
	return &clone, true
}
