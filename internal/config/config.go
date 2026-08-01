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
	defaultDBMmapSize              = int64(512 * 1024 * 1024)
	defaultDBCacheSizeKB           = int64(-64000)
	defaultDBMaxOpenConns          = 16
	defaultRateLimitPerSec         = 10
	defaultRateLimitPerMin         = 180
	defaultTrustProxy              = false
	defaultLRCLIBFallbackEnabled   = true
	defaultLRCLIBBaseURL           = "https://lrclib.net/api"
	defaultLRCLIBTimeoutMS         = 5000
	defaultMetadataFallbackEnabled = true
	defaultMusicBrainzBaseURL      = "https://musicbrainz.org/ws/2"
	defaultCoverArtArchiveBaseURL  = "https://coverartarchive.org"
	defaultMusicBrainzTimeoutMS    = 10000
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
	LRCLIBFallbackEnabled   bool
	LRCLIBBaseURL           string
	LRCLIBUserAgent         string
	LRCLIBTimeoutMS         int
	MetadataFallbackEnabled bool
	MusicBrainzBaseURL      string
	CoverArtArchiveBaseURL  string
	MusicBrainzUserAgent    string
	MusicBrainzTimeoutMS    int
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
		LRCLIBFallbackEnabled:   boolOrDefault("LRCLIB_FALLBACK_ENABLED", defaultLRCLIBFallbackEnabled),
		LRCLIBBaseURL:           valueOrDefault("LRCLIB_BASE_URL", defaultLRCLIBBaseURL),
		LRCLIBUserAgent:         valueOrDefault("LRCLIB_USER_AGENT", defaultLRCLIBUserAgent()),
		LRCLIBTimeoutMS:         intOrDefault("LRCLIB_TIMEOUT_MS", defaultLRCLIBTimeoutMS),
		MetadataFallbackEnabled: boolOrDefault("METADATA_FALLBACK_ENABLED", defaultMetadataFallbackEnabled),
		MusicBrainzBaseURL:      valueOrDefault("MUSICBRAINZ_BASE_URL", defaultMusicBrainzBaseURL),
		CoverArtArchiveBaseURL:  valueOrDefault("COVER_ART_ARCHIVE_BASE_URL", defaultCoverArtArchiveBaseURL),
		MusicBrainzUserAgent:    valueOrDefault("MUSICBRAINZ_USER_AGENT", defaultMusicBrainzUserAgent()),
		MusicBrainzTimeoutMS:    intOrDefault("MUSICBRAINZ_TIMEOUT_MS", defaultMusicBrainzTimeoutMS),
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
	if c.MetadataDBPath == c.LyricsDBPath {
		return fmt.Errorf("METADATA_DB_PATH and LYRICS_DB_PATH must be different")
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
	if c.LRCLIBTimeoutMS < 1 || c.MusicBrainzTimeoutMS < 1 {
		return fmt.Errorf("upstream timeouts must be positive")
	}
	if strings.TrimSpace(c.LRCLIBUserAgent) == "" {
		return fmt.Errorf("LRCLIB_USER_AGENT must not be empty")
	}
	for name, value := range map[string]string{"LRCLIB_BASE_URL": c.LRCLIBBaseURL, "MUSICBRAINZ_BASE_URL": c.MusicBrainzBaseURL, "COVER_ART_ARCHIVE_BASE_URL": c.CoverArtArchiveBaseURL} {
		baseURL, err := url.Parse(strings.TrimSpace(value))
		if err != nil || (baseURL.Scheme != "http" && baseURL.Scheme != "https") || baseURL.Host == "" {
			return fmt.Errorf("%s must be an http or https URL", name)
		}
	}
	if strings.TrimSpace(c.MusicBrainzUserAgent) == "" {
		return fmt.Errorf("MUSICBRAINZ_USER_AGENT must not be empty")
	}
	return nil
}

func defaultMusicBrainzUserAgent() string {
	return "music-utils/" + version.Version + " (+https://gru0.dev)"
}

func defaultLRCLIBUserAgent() string {
	return "music-utils/" + version.Version + " (+https://gru0.dev)"
}

func validateEnvironment() error {
	if err := validateIntEnv("DB_MMAP_SIZE"); err != nil {
		return err
	}
	if err := validateIntEnv("DB_CACHE_SIZE_KB"); err != nil {
		return err
	}
	for _, name := range []string{"DB_MAX_OPEN_CONNS", "RATE_LIMIT_PER_SEC", "RATE_LIMIT_PER_MIN", "LRCLIB_TIMEOUT_MS", "MUSICBRAINZ_TIMEOUT_MS"} {
		if err := validatePositiveIntEnv(name); err != nil {
			return err
		}
	}
	if err := validatePortEnv(); err != nil {
		return err
	}
	for _, name := range []string{"TRUST_PROXY", "LRCLIB_FALLBACK_ENABLED", "METADATA_FALLBACK_ENABLED"} {
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
