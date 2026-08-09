package config

import (
	"testing"

	"github.com/sillygru/music-utils/internal/version"
)

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
	if cfg.LRCLIBUserAgent != "music-utils/"+version.Version+" (+https://gru0.dev)" {
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
	if cfg.RateLimitPerSec != 20 {
		t.Fatalf("expected default per-second limit 20, got %d", cfg.RateLimitPerSec)
	}
	if cfg.RateLimitPerMin != 600 {
		t.Fatalf("expected default per-minute limit 600, got %d", cfg.RateLimitPerMin)
	}
	if cfg.TrustProxy {
		t.Fatal("expected TRUST_PROXY to default to false")
	}
}

func TestLoadFallbackDefaults(t *testing.T) {
	clearRateLimitEnv(t)
	t.Setenv("FALLBACK_PER_MIN", "")
	t.Setenv("FALLBACK_MAX_QUEUE", "")
	t.Setenv("FALLBACK_QUEUE_WAIT_MS", "")

	cfg := Load()
	if cfg.FallbackPerMin != 60 || cfg.FallbackMaxQueue != 50 || cfg.FallbackQueueWaitMS != 10000 {
		t.Fatalf("unexpected fallback defaults: %+v", cfg)
	}
}

func TestLoadCoverRefreshDefaults(t *testing.T) {
	clearRateLimitEnv(t)
	for _, name := range []string{"COVER_REFRESH_ENABLED", "COVER_REFRESH_AFTER_DAYS", "COVER_REFRESH_START_HOUR", "COVER_REFRESH_END_HOUR", "COVER_REFRESH_MAX_ROWS", "COVER_REFRESH_MAX_RECHECK"} {
		t.Setenv(name, "")
	}

	cfg := Load()
	if !cfg.CoverRefreshEnabled || cfg.CoverRefreshAfterDays != 30 || cfg.CoverRefreshStartHour != 2 || cfg.CoverRefreshEndHour != 5 {
		t.Fatalf("unexpected cover refresh defaults: %+v", cfg)
	}
	if cfg.CoverRefreshMaxRows != 2000 || cfg.CoverRefreshMaxRecheck != 200 {
		t.Fatalf("unexpected cover refresh caps: %+v", cfg)
	}
}

func TestLoadCoverRefreshMidnightStartHour(t *testing.T) {
	clearRateLimitEnv(t)
	t.Setenv("COVER_REFRESH_START_HOUR", "0")

	cfg := Load()
	if cfg.CoverRefreshStartHour != 0 {
		t.Fatalf("expected start hour 0 to be honored, got %d", cfg.CoverRefreshStartHour)
	}
}

func TestLoadRequestLogDefaults(t *testing.T) {
	clearRateLimitEnv(t)
	for _, name := range []string{"REQUEST_LOG_ENABLED", "REQUEST_LOG_DB_PATH", "REQUEST_LOG_RETENTION_DAYS", "REQUEST_LOG_UA_OPTIMIZE", "REQUEST_LOG_UA_SAVE_UNKNOWN"} {
		t.Setenv(name, "")
	}

	cfg := Load()
	if !cfg.RequestLogEnabled {
		t.Fatal("expected REQUEST_LOG_ENABLED to default to true")
	}
	if cfg.RequestLogDBPath != "./data/request_log.db" {
		t.Fatalf("unexpected default request log path: %q", cfg.RequestLogDBPath)
	}
	if cfg.RequestLogRetentionDays != 30 {
		t.Fatalf("unexpected default request log retention: %d", cfg.RequestLogRetentionDays)
	}
	if !cfg.RequestLogUAOptimize {
		t.Fatal("expected REQUEST_LOG_UA_OPTIMIZE to default to true")
	}
	if !cfg.RequestLogUASaveUnknown {
		t.Fatal("expected REQUEST_LOG_UA_SAVE_UNKNOWN to default to true")
	}
}

func TestLoadRequestLogUAOptimizationFlags(t *testing.T) {
	clearRateLimitEnv(t)
	t.Setenv("REQUEST_LOG_UA_OPTIMIZE", "false")
	t.Setenv("REQUEST_LOG_UA_SAVE_UNKNOWN", "false")

	cfg := Load()
	if cfg.RequestLogUAOptimize {
		t.Fatal("expected REQUEST_LOG_UA_OPTIMIZE=false to be honored")
	}
	if cfg.RequestLogUASaveUnknown {
		t.Fatal("expected REQUEST_LOG_UA_SAVE_UNKNOWN=false to be honored")
	}
}

func TestLoadRequestLogZeroRetentionKeepsForever(t *testing.T) {
	clearRateLimitEnv(t)
	t.Setenv("REQUEST_LOG_RETENTION_DAYS", "0")

	cfg := Load()
	if cfg.RequestLogRetentionDays != 0 {
		t.Fatalf("expected retention 0 to be honored, got %d", cfg.RequestLogRetentionDays)
	}
}

func TestLoadRequestLogNegativeOneRetentionKeepsForever(t *testing.T) {
	clearRateLimitEnv(t)
	t.Setenv("REQUEST_LOG_RETENTION_DAYS", "-1")

	cfg := Load()
	if cfg.RequestLogRetentionDays != 0 {
		t.Fatalf("expected -1 to normalize to 0 (keep forever), got %d", cfg.RequestLogRetentionDays)
	}
}

func TestLoadRequestLogDisabled(t *testing.T) {
	clearRateLimitEnv(t)
	t.Setenv("REQUEST_LOG_ENABLED", "false")

	cfg := Load()
	if cfg.RequestLogEnabled {
		t.Fatal("expected REQUEST_LOG_ENABLED=false to be honored")
	}
}

func TestLoadRequestsTodayDefaultsAndFlags(t *testing.T) {
	clearRateLimitEnv(t)
	t.Setenv("REQUEST_LOG_ENABLED", "")
	t.Setenv("REQUESTS_TODAY_ENABLED", "")

	cfg := Load()
	if cfg.RequestsTodayEnabled {
		t.Fatal("expected REQUESTS_TODAY_ENABLED to default to false")
	}

	t.Setenv("REQUESTS_TODAY_ENABLED", "true")
	if cfg := Load(); !cfg.RequestsTodayEnabled {
		t.Fatal("expected REQUESTS_TODAY_ENABLED=true to be honored")
	}

	t.Setenv("REQUESTS_TODAY_ENABLED", "false")
	if cfg := Load(); cfg.RequestsTodayEnabled {
		t.Fatal("expected REQUESTS_TODAY_ENABLED=false to be honored")
	}
}

func TestLoadStatsEndpointsDefaultDisabled(t *testing.T) {
	clearRateLimitEnv(t)
	t.Setenv("STATS_ENDPOINTS", "")

	cfg := Load()
	if len(cfg.StatsEndpoints) != 0 {
		t.Fatalf("expected STATS_ENDPOINTS to default to none served, got %v", cfg.StatsEndpoints)
	}
}

func TestLoadStatsEndpointsParsesList(t *testing.T) {
	clearRateLimitEnv(t)
	t.Setenv("STATS_ENDPOINTS", "metadata, lyrics, COVERS")

	cfg := Load()
	want := []string{"metadata", "lyrics", "covers"}
	if len(cfg.StatsEndpoints) != len(want) {
		t.Fatalf("expected endpoints %v, got %v", want, cfg.StatsEndpoints)
	}
	for i := range want {
		if cfg.StatsEndpoints[i] != want[i] {
			t.Fatalf("expected endpoint %q at index %d, got %q", want[i], i, cfg.StatsEndpoints[i])
		}
	}
}

func TestLoadStatsEndpointsAll(t *testing.T) {
	clearRateLimitEnv(t)
	t.Setenv("STATS_ENDPOINTS", "all")

	cfg := Load()
	want := allStatsEndpoints()
	if len(cfg.StatsEndpoints) != len(want) {
		t.Fatalf("expected all endpoints %v, got %v", want, cfg.StatsEndpoints)
	}
	for i := range want {
		if cfg.StatsEndpoints[i] != want[i] {
			t.Fatalf("expected endpoint %q at index %d, got %q", want[i], i, cfg.StatsEndpoints[i])
		}
	}
}

func TestLoadAndValidateRejectsUnknownStatsEndpoint(t *testing.T) {
	clearRateLimitEnv(t)
	t.Setenv("STATS_ENDPOINTS", "metadata,bogus")

	if _, err := LoadAndValidate(); err == nil {
		t.Fatal("expected an unknown STATS_ENDPOINTS token to fail validation")
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
	if cfg.RateLimitPerSec != 20 || cfg.RateLimitPerMin != 600 || cfg.TrustProxy {
		t.Fatalf("invalid values did not fall back to defaults: %+v", cfg)
	}
}
