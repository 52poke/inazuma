package httpx

import (
	"net/http"
	"net/url"
	"path"
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
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		decoded = raw
	}
	decoded = strings.ReplaceAll(decoded, " ", "_")
	decoded = path.Clean("/" + decoded)
	decoded = strings.TrimPrefix(decoded, "/")
	return decoded
}

func isSpecialPage(title string) bool {
	return strings.HasPrefix(strings.ToLower(title), "special:")
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
		keys := make(map[string]struct{}, len(q))
		for k := range q {
			keys[strings.ToLower(k)] = struct{}{}
		}
		if len(keys) == 1 {
			if _, ok := keys["title"]; ok {
				return &clone, false
			}
		}
	}
	return &clone, true
}
