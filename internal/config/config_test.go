package config

import (
	"testing"
	"time"

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

func TestLoadRichLyricsDefaults(t *testing.T) {
	clearRateLimitEnv(t)
	for _, name := range []string{"RICH_LYRICS_ENABLED", "RICH_LYRICS_BASE_URL", "RICH_LYRICS_USER_AGENT", "RICH_LYRICS_TIMEOUT_MS"} {
		t.Setenv(name, "")
	}
	cfg := Load()
	if !cfg.RichLyricsEnabled || cfg.RichLyricsBaseURL != "https://unison.boidu.dev" || cfg.RichLyricsTimeoutMS != 5000 {
		t.Fatalf("unexpected rich lyrics defaults: %+v", cfg)
	}
	if cfg.RichLyricsUserAgent != "music-utils/"+version.Version+" (+https://gru0.dev)" {
		t.Fatalf("unexpected rich lyrics user agent: %q", cfg.RichLyricsUserAgent)
	}
}

func TestLoadAndValidateRejectsInvalidRichLyricsURL(t *testing.T) {
	clearRateLimitEnv(t)
	t.Setenv("RICH_LYRICS_BASE_URL", "localhost")
	if _, err := LoadAndValidate(); err == nil {
		t.Fatal("expected invalid RICH_LYRICS_BASE_URL to fail validation")
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
	if cfg.RequestLogRetentionDays != 0 {
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

// The live and job rates are two different questions, and the operator's answer to
// one must not answer the other. A job working through a library is not a person
// waiting on an answer, so a fast live rate has to leave the job rate alone.
func TestUpstreamJobPaceIgnoresTheUserOverride(t *testing.T) {
	t.Setenv("UPSTREAM_PACE_MS", "50")
	t.Setenv("UPSTREAM_JOB_PACE_MS", "")
	t.Setenv("UPSTREAM_LYRICS_JOB_PACE_MS", "")

	cfg := Load()
	if got := cfg.UpstreamUserInterval(time.Minute); got != 50*time.Millisecond {
		t.Fatalf("user interval = %s, want the 50ms override", got)
	}
	if got := cfg.UpstreamJobInterval(); got != 2*time.Second {
		t.Fatalf("job interval = %s, want 2s: a live override must not reach the job", got)
	}
	if got := cfg.UpstreamLyricsJobInterval(); got != 5*time.Second {
		t.Fatalf("lyrics job interval = %s, want 5s: a live override must not reach the job", got)
	}
}

// The two jobs ask the same upstreams for different things and pay for it in
// different numbers of requests, so their rates are two settings rather than one.
// An operator tuning the metadata job must not slow the lyrics pass, or speed it
// up, by editing the number they did not mean to change.
func TestUpstreamJobPacesAreIndependent(t *testing.T) {
	t.Setenv("UPSTREAM_JOB_PACE_MS", "")
	t.Setenv("UPSTREAM_LYRICS_JOB_PACE_MS", "7000")

	cfg := Load()
	if got := cfg.UpstreamJobInterval(); got != 2*time.Second {
		t.Fatalf("metadata job interval = %s, want the 2s default: setting the lyrics rate must not reach it", got)
	}
	if got := cfg.UpstreamLyricsJobInterval(); got != 7*time.Second {
		t.Fatalf("lyrics job interval = %s, want 7s", got)
	}

	t.Setenv("UPSTREAM_JOB_PACE_MS", "3000")
	cfg = Load()
	if got := cfg.UpstreamJobInterval(); got != 3*time.Second {
		t.Fatalf("metadata job interval = %s, want 3s", got)
	}
	if got := cfg.UpstreamLyricsJobInterval(); got != 7*time.Second {
		t.Fatalf("lyrics job interval = %s, want its own 7s: setting the metadata rate must not reach it", got)
	}
}

func TestUpstreamPaceDefaultsToPerProviderIntervals(t *testing.T) {
	t.Setenv("UPSTREAM_PACE_MS", "")
	t.Setenv("UPSTREAM_JOB_PACE_MS", "")
	t.Setenv("UPSTREAM_LYRICS_JOB_PACE_MS", "")

	cfg := Load()
	if cfg.UpstreamPaceMS != 0 {
		t.Fatalf("UpstreamPaceMS = %d, want 0 (unset) so each upstream keeps its own rate", cfg.UpstreamPaceMS)
	}
	// Each provider's own constant has to survive untouched, or one global number
	// would quietly replace rates chosen per upstream.
	if got := cfg.UpstreamUserInterval(200 * time.Millisecond); got != 200*time.Millisecond {
		t.Fatalf("user interval = %s, want the provider's own 200ms", got)
	}
	if got := cfg.UpstreamUserInterval(500 * time.Millisecond); got != 500*time.Millisecond {
		t.Fatalf("user interval = %s, want the provider's own 500ms", got)
	}
	if got := cfg.UpstreamJobInterval(); got != 2*time.Second {
		t.Fatalf("job interval = %s, want 2s", got)
	}
	if got := cfg.UpstreamLyricsJobInterval(); got != 5*time.Second {
		t.Fatalf("lyrics job interval = %s, want 5s: it asks every provider about every song", got)
	}
}

func TestUpstreamPaceOverridesEveryProvider(t *testing.T) {
	t.Setenv("UPSTREAM_PACE_MS", "750")

	cfg := Load()
	if cfg.UpstreamPaceMS != 750 {
		t.Fatalf("UpstreamPaceMS = %d, want 750", cfg.UpstreamPaceMS)
	}
	for _, providerDefault := range []time.Duration{200 * time.Millisecond, 300 * time.Millisecond, 500 * time.Millisecond, 2 * time.Second} {
		if got := cfg.UpstreamUserInterval(providerDefault); got != 750*time.Millisecond {
			t.Fatalf("user interval = %s, want 750ms regardless of the %s provider default", got, providerDefault)
		}
	}
}

// A pace of zero means "do not wait at all", so every path that can produce one
// has to refuse it. Getting this wrong does not fail loudly, it removes the
// ceiling on an upstream and the run simply floods it.
func TestUpstreamPaceNeverResolvesToNoPacing(t *testing.T) {
	for _, value := range []string{"0", "-1", "not-a-number", ""} {
		t.Run("user="+value, func(t *testing.T) {
			t.Setenv("UPSTREAM_PACE_MS", value)
			cfg := Load()
			if got := cfg.UpstreamUserInterval(200 * time.Millisecond); got != 200*time.Millisecond {
				t.Fatalf("user interval = %s, want the 200ms provider default: %q must read as unset", got, value)
			}
		})
		t.Run("job="+value, func(t *testing.T) {
			t.Setenv("UPSTREAM_JOB_PACE_MS", value)
			cfg := Load()
			if got := cfg.UpstreamJobInterval(); got != 2*time.Second {
				t.Fatalf("job interval = %s, want the 2s default: %q must not disable pacing", got, value)
			}
		})
		t.Run("lyrics-job="+value, func(t *testing.T) {
			t.Setenv("UPSTREAM_LYRICS_JOB_PACE_MS", value)
			cfg := Load()
			if got := cfg.UpstreamLyricsJobInterval(); got != 5*time.Second {
				t.Fatalf("lyrics job interval = %s, want the 5s default: %q must not disable pacing", got, value)
			}
		})
	}
}

// Config is built by hand in plenty of places that never call Load, so the
// accessor has to be safe on a zero value rather than only on a loaded one.
func TestUpstreamJobIntervalOnZeroValueConfig(t *testing.T) {
	var cfg Config
	if got := cfg.UpstreamJobInterval(); got != 2*time.Second {
		t.Fatalf("job interval = %s, want 2s: an unloaded Config must not mean no pacing", got)
	}
	if got := cfg.UpstreamUserInterval(300 * time.Millisecond); got != 300*time.Millisecond {
		t.Fatalf("user interval = %s, want the 300ms provider default", got)
	}
}
