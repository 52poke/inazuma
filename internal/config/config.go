package config

import (
	"errors"
	"os"
	"strconv"
)

type Config struct {
	ListenAddr         string
	MediaWikiBaseURL   string
	RedisAddr          string
	RedisDB            int
	RedisPassword      string
	S3Endpoint         string
	S3Region           string
	S3Bucket           string
	S3AccessKey        string
	S3SecretKey        string
	NginxPurgeURL      string
	LoggedInCookieName string
	CacheTTLSeconds    int
	LockTTLSeconds     int
	MaxLockWaitSeconds int
}

func Load() (Config, error) {
	cfg := Config{
		ListenAddr:         getenv("INAZUMA_LISTEN_ADDR", ":8080"),
		MediaWikiBaseURL:   getenv("INAZUMA_MEDIAWIKI_BASE_URL", ""),
		RedisAddr:          getenv("INAZUMA_REDIS_ADDR", ""),
		RedisDB:            getenvInt("INAZUMA_REDIS_DB", 0),
		RedisPassword:      os.Getenv("INAZUMA_REDIS_PASSWORD"),
		S3Endpoint:         getenv("INAZUMA_S3_ENDPOINT", ""),
		S3Region:           getenv("INAZUMA_S3_REGION", ""),
		S3Bucket:           getenv("INAZUMA_S3_BUCKET", ""),
		S3AccessKey:        os.Getenv("INAZUMA_S3_ACCESS_KEY"),
		S3SecretKey:        os.Getenv("INAZUMA_S3_SECRET_KEY"),
		NginxPurgeURL:      getenv("INAZUMA_NGINX_PURGE_URL", ""),
		LoggedInCookieName: getenv("INAZUMA_LOGGED_IN_COOKIE", "52poke_wikiUserID"),
		CacheTTLSeconds:    getenvInt("INAZUMA_CACHE_TTL_SECONDS", 2592000),
		LockTTLSeconds:     getenvInt("INAZUMA_LOCK_TTL_SECONDS", 45),
		MaxLockWaitSeconds: getenvInt("INAZUMA_MAX_LOCK_WAIT_SECONDS", 3),
	}

	if cfg.MediaWikiBaseURL == "" {
		return cfg, errors.New("INAZUMA_MEDIAWIKI_BASE_URL is required")
	}
	if cfg.RedisAddr == "" {
		return cfg, errors.New("INAZUMA_REDIS_ADDR is required")
	}
	if cfg.S3Endpoint == "" || cfg.S3Bucket == "" || cfg.S3AccessKey == "" || cfg.S3SecretKey == "" {
		return cfg, errors.New("S3 endpoint/bucket/access/secret are required")
	}
	return cfg, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
