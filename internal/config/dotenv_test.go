package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnv(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	content := `
# Sample dotenv configuration
PORT=9090
REQUEST_LOG_DB_PATH=/custom/data/request_log.db
LOG_LEVEL="debug"
TRUST_PROXY='true'
export LRCLIB_TIMEOUT_MS=7000
`
	if err := os.WriteFile(envPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	t.Setenv("PORT", "")
	t.Setenv("REQUEST_LOG_DB_PATH", "")
	t.Setenv("LOG_LEVEL", "")
	t.Setenv("TRUST_PROXY", "")
	t.Setenv("LRCLIB_TIMEOUT_MS", "")

	if err := LoadDotEnv(envPath); err != nil {
		t.Fatalf("LoadDotEnv failed: %v", err)
	}

	if os.Getenv("PORT") != "9090" {
		t.Errorf("expected PORT=9090, got %q", os.Getenv("PORT"))
	}
	if os.Getenv("REQUEST_LOG_DB_PATH") != "/custom/data/request_log.db" {
		t.Errorf("expected REQUEST_LOG_DB_PATH=/custom/data/request_log.db, got %q", os.Getenv("REQUEST_LOG_DB_PATH"))
	}
	if os.Getenv("LOG_LEVEL") != "debug" {
		t.Errorf("expected LOG_LEVEL=debug, got %q", os.Getenv("LOG_LEVEL"))
	}
	if os.Getenv("TRUST_PROXY") != "true" {
		t.Errorf("expected TRUST_PROXY=true, got %q", os.Getenv("TRUST_PROXY"))
	}
	if os.Getenv("LRCLIB_TIMEOUT_MS") != "7000" {
		t.Errorf("expected LRCLIB_TIMEOUT_MS=7000, got %q", os.Getenv("LRCLIB_TIMEOUT_MS"))
	}
}

func TestLoadDotEnvDoesNotOverwriteExistingEnv(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	content := `PORT=9999`
	if err := os.WriteFile(envPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	t.Setenv("PORT", "1234")
	if err := LoadDotEnv(envPath); err != nil {
		t.Fatalf("LoadDotEnv failed: %v", err)
	}

	if os.Getenv("PORT") != "1234" {
		t.Errorf("expected PORT to remain 1234, got %q", os.Getenv("PORT"))
	}
}

func TestLoadDotEnvMissingFileIgnored(t *testing.T) {
	if err := LoadDotEnv("/non/existent/.env"); err != nil {
		t.Fatalf("expected missing file to be ignored, got: %v", err)
	}
}
