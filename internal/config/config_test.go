package config

import "testing"

func TestLoadTimeoutDefaults(t *testing.T) {
	setRequiredEnvironment(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got, want := cfg.PurgeMediaWikiTimeoutSeconds, 40; got != want {
		t.Errorf("PurgeMediaWikiTimeoutSeconds = %d, want %d", got, want)
	}
	if got, want := cfg.ServerWriteTimeoutSeconds, 130; got != want {
		t.Errorf("ServerWriteTimeoutSeconds = %d, want %d", got, want)
	}
	if got, want := cfg.LockTTLSeconds, 60; got != want {
		t.Errorf("LockTTLSeconds = %d, want %d", got, want)
	}
}

func TestLoadRejectsNonPositiveTimeouts(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{name: "purge MediaWiki timeout", key: "INAZUMA_PURGE_MEDIAWIKI_TIMEOUT_SECONDS"},
		{name: "server write timeout", key: "INAZUMA_SERVER_WRITE_TIMEOUT_SECONDS"},
		{name: "lock TTL", key: "INAZUMA_LOCK_TTL_SECONDS"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setRequiredEnvironment(t)
			t.Setenv(test.key, "0")
			if _, err := Load(); err == nil {
				t.Fatalf("Load() error = nil, want validation error for %s", test.key)
			}
		})
	}
}

func setRequiredEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("INAZUMA_PURGE_MEDIAWIKI_TIMEOUT_SECONDS", "")
	t.Setenv("INAZUMA_SERVER_WRITE_TIMEOUT_SECONDS", "")
	t.Setenv("INAZUMA_LOCK_TTL_SECONDS", "")
	t.Setenv("INAZUMA_MEDIAWIKI_BASE_URL", "http://mediawiki.test")
	t.Setenv("INAZUMA_REDIS_ADDR", "redis.test:6379")
	t.Setenv("INAZUMA_S3_ENDPOINT", "https://s3.test")
	t.Setenv("INAZUMA_S3_BUCKET", "cache")
	t.Setenv("INAZUMA_S3_ACCESS_KEY", "access")
	t.Setenv("INAZUMA_S3_SECRET_KEY", "secret")
}
