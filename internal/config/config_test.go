package config

import "testing"

func clearRateLimitEnv(t *testing.T) {
	t.Helper()
	t.Setenv("RATE_LIMIT_PER_SEC", "")
	t.Setenv("RATE_LIMIT_PER_MIN", "")
	t.Setenv("TRUST_PROXY", "")
}

func TestLoadLRCLIBDefaults(t *testing.T) {
	clearRateLimitEnv(t)
	t.Setenv("LRCLIB_FALLBACK_ENABLED", "")
	t.Setenv("LRCLIB_BASE_URL", "")
	t.Setenv("LRCLIB_USER_AGENT", "")
	t.Setenv("LRCLIB_TIMEOUT_MS", "")

	cfg := Load()
	if !cfg.LRCLIBFallbackEnabled || cfg.LRCLIBBaseURL != "https://lrclib.net/api" || cfg.LRCLIBTimeoutMS != 5000 {
		t.Fatalf("unexpected LRCLIB defaults: %+v", cfg)
	}
	if cfg.LRCLIBUserAgent != "music-utils/v0.1.0 (+https://gru0.dev)" {
		t.Fatalf("unexpected default LRCLIB user agent: %q", cfg.LRCLIBUserAgent)
	}
}

func TestLoadAndValidateRejectsInvalidSuppliedValues(t *testing.T) {
	t.Setenv("PORT", "not-a-port")
	if _, err := LoadAndValidate(); err == nil {
		t.Fatal("expected invalid PORT to fail validation")
	}
	t.Setenv("PORT", "")
	t.Setenv("LRCLIB_BASE_URL", "localhost")
	if _, err := LoadAndValidate(); err == nil {
		t.Fatal("expected invalid LRCLIB_BASE_URL to fail validation")
	}
}

func TestLoadRateLimitDefaults(t *testing.T) {
	clearRateLimitEnv(t)

	cfg := Load()
	if cfg.RateLimitPerSec != 10 {
		t.Fatalf("expected default per-second limit 10, got %d", cfg.RateLimitPerSec)
	}
	if cfg.RateLimitPerMin != 180 {
		t.Fatalf("expected default per-minute limit 180, got %d", cfg.RateLimitPerMin)
	}
	if cfg.TrustProxy {
		t.Fatal("expected TRUST_PROXY to default to false")
	}
}

func TestLoadRateLimitValuesAndFallbacks(t *testing.T) {
	t.Setenv("RATE_LIMIT_PER_SEC", "7")
	t.Setenv("RATE_LIMIT_PER_MIN", "91")
	t.Setenv("TRUST_PROXY", "true")

	cfg := Load()
	if cfg.RateLimitPerSec != 7 || cfg.RateLimitPerMin != 91 || !cfg.TrustProxy {
		t.Fatalf("unexpected configured rate limits: %+v", cfg)
	}

	t.Setenv("RATE_LIMIT_PER_SEC", "0")
	t.Setenv("RATE_LIMIT_PER_MIN", "not-a-number")
	t.Setenv("TRUST_PROXY", "not-a-boolean")
	cfg = Load()
	if cfg.RateLimitPerSec != 10 || cfg.RateLimitPerMin != 180 || cfg.TrustProxy {
		t.Fatalf("invalid values did not fall back to defaults: %+v", cfg)
	}
}
