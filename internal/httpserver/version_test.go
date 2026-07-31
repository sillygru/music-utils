package httpserver

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sillygru/music-utils/internal/version"
)

func TestVersionEndpoint(t *testing.T) {
	database := testHTTPDatabase(t)
	server := New("8080", database)
	cleanupHTTPServer(t, server)

	response := performRequest(t, server.Handler, "/version")
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}

	var body struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode version response: %v", err)
	}
	if body.Version != version.Version {
		t.Fatalf("expected version %q, got %q", version.Version, body.Version)
	}

	health := performRequest(t, server.Handler, "/healthz")
	if health.Code != http.StatusOK || health.Body.String() != "{\"status\":\"ok\"}\n" {
		t.Fatalf("health endpoint contract changed: status=%d body=%q", health.Code, health.Body.String())
	}
}
