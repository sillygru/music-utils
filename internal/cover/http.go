package cover

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const maxResponseBytes = 4 << 20

// intervalLimiter paces upstream requests so the slowest honored source's rate
// limit is never exceeded. The whole chain shares one limiter so concurrent
// lookups cannot burst one source past its budget.
type intervalLimiter struct {
	mu       sync.Mutex
	interval time.Duration
	last     time.Time
}

// Wait blocks until the caller may issue its next request. The first call is
// never delayed but still records its timestamp so subsequent calls space out.
func (l *intervalLimiter) Wait(ctx context.Context) error {
	if l.interval <= 0 {
		return nil
	}
	l.mu.Lock()
	if l.last.IsZero() {
		l.last = time.Now()
		l.mu.Unlock()
		return nil
	}
	wait := time.Until(l.last.Add(l.interval))
	l.mu.Unlock()
	if wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	l.mu.Lock()
	l.last = time.Now()
	l.mu.Unlock()
	return nil
}

// jsonClient performs authenticated-by-absence JSON GETs shared by iTunes and
// Deezer. Non-2xx responses return an error rather than an empty result.
type jsonClient struct {
	client *http.Client
	agent  string
	rate   *intervalLimiter
}

func (c *jsonClient) get(ctx context.Context, endpoint string, value any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", c.agent)
	req.Header.Set("Accept", "application/json")
	if c.rate != nil {
		if err := c.rate.Wait(ctx); err != nil {
			return err
		}
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("provider returned HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(value); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// encodeQuery builds a URL with the given query params on a base endpoint.
func encodeQuery(base, path string, params url.Values) (string, error) {
	endpoint, err := url.Parse(strings.TrimRight(strings.TrimSpace(base), "/") + path)
	if err != nil {
		return "", err
	}
	endpoint.RawQuery = params.Encode()
	return endpoint.String(), nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
