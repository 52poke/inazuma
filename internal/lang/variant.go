package lang

import (
	"strconv"
	"strings"
)

const (
	VariantZH   = "zh"
	VariantHans = "zh-hans"
	VariantHant = "zh-hant"
)

var simplifiedTags = map[string]struct{}{
	"zh-cn":   {},
	"zh-hans": {},
	"zh-sg":   {},
	"zh-my":   {},
}

var traditionalTags = map[string]struct{}{
	"zh-hk":   {},
	"zh-tw":   {},
	"zh-mo":   {},
	"zh-hant": {},
}

func VariantFromAcceptLanguage(header string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return VariantZH
	}

	maxHans := -1.0
	maxHant := -1.0

	parts := strings.Split(header, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		langTag, q := parseLangPart(part)
		if langTag == "" {
			continue
		}

		if _, ok := simplifiedTags[langTag]; ok {
			if q > maxHans {
				maxHans = q
			}
			continue
		}
		if _, ok := traditionalTags[langTag]; ok {
			if q > maxHant {
				maxHant = q
			}
		}
	}

	hasHans := maxHans >= 0
	hasHant := maxHant >= 0

	switch {
	case hasHans && !hasHant:
		return VariantHans
	case hasHant && !hasHans:
		return VariantHant
	case hasHans && hasHant:
		if maxHans > maxHant {
			return VariantHans
		}
		if maxHant > maxHans {
			return VariantHant
		}
		return VariantZH
	default:
		return VariantZH
	}
}

func parseLangPart(part string) (string, float64) {
	langTag := part
	q := 1.0

	if idx := strings.Index(part, ";"); idx != -1 {
		langTag = strings.TrimSpace(part[:idx])
		params := strings.Split(part[idx+1:], ";")
		for _, p := range params {
			p = strings.TrimSpace(p)
			if strings.HasPrefix(strings.ToLower(p), "q=") {
				val := strings.TrimSpace(p[2:])
				if v, err := strconv.ParseFloat(val, 64); err == nil {
					q = v
				}
			}
		}
	}

	langTag = strings.ToLower(strings.TrimSpace(langTag))
	return langTag, q
}
