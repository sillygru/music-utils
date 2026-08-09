package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/sillygru/music-utils/internal/version"
)

const (
	defaultPort                    = "8080"
	defaultLogLevel                = "info"
	defaultMetadataDBPath          = "./data/metadata.db"
	defaultLyricsDBPath            = "./data/lyrics.db"
	defaultCoverDBPath             = "./data/cover.db"
	defaultDBMmapSize              = int64(512 * 1024 * 1024)
	defaultDBCacheSizeKB           = int64(-64000)
	defaultDBMaxOpenConns          = 16
	defaultRateLimitPerSec         = 20
	defaultRateLimitPerMin         = 600
	defaultTrustProxy              = false
	defaultFallbackPerMin          = 60
	defaultFallbackMaxQueue        = 50
	defaultFallbackQueueWaitMS     = 10000
	defaultCoverRefreshEnabled     = true
	defaultCoverRefreshAfterDays   = 30
	defaultCoverRefreshStartHour   = 2
	defaultCoverRefreshEndHour     = 5
	defaultCoverRefreshMaxRows     = 2000
	defaultCoverRefreshMaxRecheck  = 200
	defaultRequestLogEnabled       = true
	defaultRequestLogDBPath        = "./data/request_log.db"
	defaultRequestLogRetentionDays = 30
	defaultLRCLIBFallbackEnabled   = true
	defaultLRCLIBBaseURL           = "https://lrclib.net/api"
	defaultLRCLIBTimeoutMS         = 5000
	defaultMetadataFallbackEnabled = true
	defaultITunesBaseURL           = "https://itunes.apple.com"
	defaultDeezerBaseURL           = "https://api.deezer.com"
	defaultMetadataTimeoutMS       = 5000
	defaultCoverFallbackEnabled    = true
	defaultLastfmBaseURL           = "https://www.last.fm"
	defaultCoverTimeoutMS          = 10000
)

// Config contains the settings needed to start the server.
type Config struct {
	Port                    string
	LogLevel                string
	MetadataDBPath          string
	LyricsDBPath            string
	DBMmapSize              int64
	DBCacheSizeKB           int64
	DBMaxOpenConns          int
	RateLimitPerSec         int
	RateLimitPerMin         int
	TrustProxy              bool
	FallbackPerMin          int
	FallbackMaxQueue        int
	FallbackQueueWaitMS     int
	CoverRefreshEnabled     bool
	CoverRefreshAfterDays   int
	CoverRefreshStartHour   int
	CoverRefreshEndHour     int
	CoverRefreshMaxRows     int
	CoverRefreshMaxRecheck  int
	RequestLogEnabled       bool
	RequestLogDBPath        string
	RequestLogRetentionDays int
	LRCLIBFallbackEnabled   bool
	LRCLIBBaseURL           string
	LRCLIBUserAgent         string
	LRCLIBTimeoutMS         int
	MetadataFallbackEnabled bool
	ITunesBaseURL           string
	DeezerBaseURL           string
	MetadataTimeoutMS       int
	MetadataUserAgent       string

	CoverDBPath          string
	CoverFallbackEnabled bool
	LastfmBaseURL        string
	CoverTimeoutMS       int
	CoverUserAgent       string
}

// Load reads configuration from the environment and applies defaults when a
// setting is not provided or cannot be parsed.
func Load() Config {
	return Config{
		Port:                    valueOrDefault("PORT", defaultPort),
		LogLevel:                valueOrDefault("LOG_LEVEL", defaultLogLevel),
		MetadataDBPath:          valueOrDefault("METADATA_DB_PATH", defaultMetadataDBPath),
		LyricsDBPath:            valueOrDefault("LYRICS_DB_PATH", defaultLyricsDBPath),
		DBMmapSize:              int64OrDefault("DB_MMAP_SIZE", defaultDBMmapSize),
		DBCacheSizeKB:           int64OrDefault("DB_CACHE_SIZE_KB", defaultDBCacheSizeKB),
		DBMaxOpenConns:          intOrDefault("DB_MAX_OPEN_CONNS", defaultDBMaxOpenConns),
		RateLimitPerSec:         intOrDefault("RATE_LIMIT_PER_SEC", defaultRateLimitPerSec),
		RateLimitPerMin:         intOrDefault("RATE_LIMIT_PER_MIN", defaultRateLimitPerMin),
		TrustProxy:              boolOrDefault("TRUST_PROXY", defaultTrustProxy),
		FallbackPerMin:          intOrDefault("FALLBACK_PER_MIN", defaultFallbackPerMin),
		FallbackMaxQueue:        intOrDefault("FALLBACK_MAX_QUEUE", defaultFallbackMaxQueue),
		FallbackQueueWaitMS:     intOrDefault("FALLBACK_QUEUE_WAIT_MS", defaultFallbackQueueWaitMS),
		CoverRefreshEnabled:     boolOrDefault("COVER_REFRESH_ENABLED", defaultCoverRefreshEnabled),
		CoverRefreshAfterDays:   intOrDefault("COVER_REFRESH_AFTER_DAYS", defaultCoverRefreshAfterDays),
		CoverRefreshStartHour:   hourOrDefault("COVER_REFRESH_START_HOUR", defaultCoverRefreshStartHour),
		CoverRefreshEndHour:     hourOrDefault("COVER_REFRESH_END_HOUR", defaultCoverRefreshEndHour),
		CoverRefreshMaxRows:     intOrDefault("COVER_REFRESH_MAX_ROWS", defaultCoverRefreshMaxRows),
		CoverRefreshMaxRecheck:  intOrDefault("COVER_REFRESH_MAX_RECHECK", defaultCoverRefreshMaxRecheck),
		RequestLogEnabled:       boolOrDefault("REQUEST_LOG_ENABLED", defaultRequestLogEnabled),
		RequestLogDBPath:        valueOrDefault("REQUEST_LOG_DB_PATH", defaultRequestLogDBPath),
		RequestLogRetentionDays: nonNegativeIntOrDefault("REQUEST_LOG_RETENTION_DAYS", defaultRequestLogRetentionDays),
		LRCLIBFallbackEnabled:   boolOrDefault("LRCLIB_FALLBACK_ENABLED", defaultLRCLIBFallbackEnabled),
		LRCLIBBaseURL:           valueOrDefault("LRCLIB_BASE_URL", defaultLRCLIBBaseURL),
		LRCLIBUserAgent:         valueOrDefault("LRCLIB_USER_AGENT", defaultLRCLIBUserAgent()),
		LRCLIBTimeoutMS:         intOrDefault("LRCLIB_TIMEOUT_MS", defaultLRCLIBTimeoutMS),
		MetadataFallbackEnabled: boolOrDefault("METADATA_FALLBACK_ENABLED", defaultMetadataFallbackEnabled),
		ITunesBaseURL:           valueOrDefault("ITUNES_BASE_URL", defaultITunesBaseURL),
		DeezerBaseURL:           valueOrDefault("DEEZER_BASE_URL", defaultDeezerBaseURL),
		MetadataTimeoutMS:       intOrDefault("METADATA_TIMEOUT_MS", defaultMetadataTimeoutMS),
		MetadataUserAgent:       valueOrDefault("METADATA_USER_AGENT", defaultMetadataUserAgent()),

		CoverDBPath:          valueOrDefault("COVER_DB_PATH", defaultCoverDBPath),
		CoverFallbackEnabled: boolOrDefault("COVER_FALLBACK_ENABLED", defaultCoverFallbackEnabled),
		LastfmBaseURL:        valueOrDefault("LASTFM_BASE_URL", defaultLastfmBaseURL),
		CoverTimeoutMS:       intOrDefault("COVER_TIMEOUT_MS", defaultCoverTimeoutMS),
		CoverUserAgent:       valueOrDefault("COVER_USER_AGENT", defaultCoverUserAgent()),
	}
}

// LoadAndValidate loads environment configuration and fails when a supplied
// value is malformed or the resulting configuration is unsafe to run.
func LoadAndValidate() (Config, error) {
	cfg := Load()
	if err := validateEnvironment(); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks values that affect server, database, and upstream behavior.
func (c Config) Validate() error {
	port, err := strconv.Atoi(strings.TrimSpace(c.Port))
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("PORT must be a number between 1 and 65535")
	}
	if strings.TrimSpace(c.LogLevel) != "debug" && strings.TrimSpace(c.LogLevel) != "info" && strings.TrimSpace(c.LogLevel) != "warn" && strings.TrimSpace(c.LogLevel) != "error" {
		return fmt.Errorf("LOG_LEVEL must be one of debug, info, warn, or error")
	}
	if strings.TrimSpace(c.MetadataDBPath) == "" {
		return fmt.Errorf("METADATA_DB_PATH must not be empty")
	}
	if strings.TrimSpace(c.LyricsDBPath) == "" {
		return fmt.Errorf("LYRICS_DB_PATH must not be empty")
	}
	if strings.TrimSpace(c.CoverDBPath) == "" {
		return fmt.Errorf("COVER_DB_PATH must not be empty")
	}
	if c.MetadataDBPath == c.LyricsDBPath || c.MetadataDBPath == c.CoverDBPath || c.LyricsDBPath == c.CoverDBPath {
		return fmt.Errorf("METADATA_DB_PATH, LYRICS_DB_PATH, and COVER_DB_PATH must be different")
	}
	if c.DBMmapSize <= 0 {
		return fmt.Errorf("DB_MMAP_SIZE must be positive")
	}
	if c.DBMaxOpenConns < 1 {
		return fmt.Errorf("DB_MAX_OPEN_CONNS must be positive")
	}
	if c.RateLimitPerSec < 1 || c.RateLimitPerMin < 1 {
		return fmt.Errorf("rate limits must be positive")
	}
	if c.FallbackPerMin < 1 {
		return fmt.Errorf("FALLBACK_PER_MIN must be positive")
	}
	if c.FallbackMaxQueue < 1 {
		return fmt.Errorf("FALLBACK_MAX_QUEUE must be positive")
	}
	if c.FallbackQueueWaitMS < 1000 {
		return fmt.Errorf("FALLBACK_QUEUE_WAIT_MS must be at least 1000")
	}
	if c.CoverRefreshAfterDays < 1 {
		return fmt.Errorf("COVER_REFRESH_AFTER_DAYS must be positive")
	}
	if c.CoverRefreshStartHour < 0 || c.CoverRefreshStartHour > 23 || c.CoverRefreshEndHour < 0 || c.CoverRefreshEndHour > 23 {
		return fmt.Errorf("COVER_REFRESH_START_HOUR and COVER_REFRESH_END_HOUR must be between 0 and 23")
	}
	if c.CoverRefreshMaxRows < 1 {
		return fmt.Errorf("COVER_REFRESH_MAX_ROWS must be positive")
	}
	if c.CoverRefreshMaxRecheck < 1 {
		return fmt.Errorf("COVER_REFRESH_MAX_RECHECK must be positive")
	}
	if strings.TrimSpace(c.RequestLogDBPath) == "" {
		return fmt.Errorf("REQUEST_LOG_DB_PATH must not be empty")
	}
	if c.RequestLogRetentionDays < 0 {
		return fmt.Errorf("REQUEST_LOG_RETENTION_DAYS must be zero or positive")
	}
	if c.LRCLIBTimeoutMS < 1 || c.MetadataTimeoutMS < 1 || c.CoverTimeoutMS < 1 {
		return fmt.Errorf("upstream timeouts must be positive")
	}
	if strings.TrimSpace(c.LRCLIBUserAgent) == "" {
		return fmt.Errorf("LRCLIB_USER_AGENT must not be empty")
	}
	for name, value := range map[string]string{"LRCLIB_BASE_URL": c.LRCLIBBaseURL, "ITUNES_BASE_URL": c.ITunesBaseURL, "DEEZER_BASE_URL": c.DeezerBaseURL, "LASTFM_BASE_URL": c.LastfmBaseURL} {
		baseURL, err := url.Parse(strings.TrimSpace(value))
		if err != nil || (baseURL.Scheme != "http" && baseURL.Scheme != "https") || baseURL.Host == "" {
			return fmt.Errorf("%s must be an http or https URL", name)
		}
	}
	if strings.TrimSpace(c.MetadataUserAgent) == "" {
		return fmt.Errorf("METADATA_USER_AGENT must not be empty")
	}
	if strings.TrimSpace(c.CoverUserAgent) == "" {
		return fmt.Errorf("COVER_USER_AGENT must not be empty")
	}
	return nil
}

func defaultMetadataUserAgent() string {
	return "music-utils/" + version.Version + " (+https://gru0.dev)"
}

func defaultLRCLIBUserAgent() string {
	return "music-utils/" + version.Version + " (+https://gru0.dev)"
}

func defaultCoverUserAgent() string {
	return "music-utils/" + version.Version + " (+https://gru0.dev)"
}

func validateEnvironment() error {
	if err := validateIntEnv("DB_MMAP_SIZE"); err != nil {
		return err
	}
	if err := validateIntEnv("DB_CACHE_SIZE_KB"); err != nil {
		return err
	}
	for _, name := range []string{"DB_MAX_OPEN_CONNS", "RATE_LIMIT_PER_SEC", "RATE_LIMIT_PER_MIN", "FALLBACK_PER_MIN", "FALLBACK_MAX_QUEUE", "FALLBACK_QUEUE_WAIT_MS", "COVER_REFRESH_AFTER_DAYS", "COVER_REFRESH_MAX_ROWS", "COVER_REFRESH_MAX_RECHECK", "LRCLIB_TIMEOUT_MS", "METADATA_TIMEOUT_MS", "COVER_TIMEOUT_MS"} {
		if err := validatePositiveIntEnv(name); err != nil {
			return err
		}
	}
	if err := validatePortEnv(); err != nil {
		return err
	}
	if value := strings.TrimSpace(os.Getenv("REQUEST_LOG_RETENTION_DAYS")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 {
			return fmt.Errorf("REQUEST_LOG_RETENTION_DAYS must be a non-negative integer")
		}
	}
	for _, name := range []string{"TRUST_PROXY", "LRCLIB_FALLBACK_ENABLED", "METADATA_FALLBACK_ENABLED", "COVER_FALLBACK_ENABLED", "COVER_REFRESH_ENABLED", "REQUEST_LOG_ENABLED"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			if _, err := strconv.ParseBool(value); err != nil {
				return fmt.Errorf("%s must be a boolean", name)
			}
		}
	}
	return nil
}

func validatePortEnv() error {
	value := strings.TrimSpace(os.Getenv("PORT"))
	if value == "" {
		return nil
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("PORT must be a number between 1 and 65535")
	}
	return nil
}

func validateIntEnv(name string) error {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return nil
	}
	if _, err := strconv.ParseInt(value, 10, 64); err != nil {
		return fmt.Errorf("%s must be an integer", name)
	}
	return nil
}

func validatePositiveIntEnv(name string) error {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		return fmt.Errorf("%s must be a positive integer", name)
	}
	return nil
}

func valueOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func int64OrDefault(name string, fallback int64) int64 {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func intOrDefault(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		return fallback
	}
	return parsed
}

// nonNegativeIntOrDefault reads an int env value, allowing 0. Negative or
// unparseable values fall back to the default. Used by
// REQUEST_LOG_RETENTION_DAYS where 0 means keep request logs forever.
func nonNegativeIntOrDefault(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}

// hourOrDefault reads a 0-23 hour value, allowing 0 (midnight), which
// intOrDefault would otherwise reject as below its positive floor.
func hourOrDefault(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func boolOrDefault(name string, fallback bool) bool {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}
