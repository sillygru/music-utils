package httpserver

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRecoveryReturnsJSON500AndOneRequestLog(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	handler := requestLogger(
		recoverMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic("deliberate test panic")
		}), logger),
		logger,
		nil,
	)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/panic", nil))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", response.Code)
	}
	var body apiError
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode panic response: %v", err)
	}
	if body != (apiError{Code: 500, Message: "Internal server error"}) {
		t.Fatalf("unexpected panic response: %+v", body)
	}
	if count := strings.Count(logs.String(), `"msg":"request"`); count != 1 {
		t.Fatalf("expected one request log record, got %d: %s", count, logs.String())
	}
	if !strings.Contains(logs.String(), `"outcome":"error"`) {
		t.Fatalf("request log did not contain error outcome: %s", logs.String())
	}
}
