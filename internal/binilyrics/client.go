package binilyrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sillygru/music-utils/internal/names"
	"github.com/sillygru/music-utils/internal/pacer"
)

var ErrNotFound = errors.New("BiniLyrics not found")

type Result struct {
	Content  string
	SyncType string
	Source   string
}

type Client struct {
	baseURL string
	agent   string
	http    *http.Client
	pace    *pacer.Pacer
}

func New(baseURL, agent string, timeout time.Duration) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("invalid BiniLyrics base URL")
	}
	if strings.TrimSpace(agent) == "" || timeout <= 0 {
		return nil, fmt.Errorf("BiniLyrics user agent and timeout are required")
	}
	return &Client{baseURL: baseURL, agent: agent, http: &http.Client{Timeout: timeout}, pace: pacer.New(200 * time.Millisecond)}, nil
}

func (c *Client) Get(ctx context.Context, song, artist, album string, duration float64) (*Result, error) {
	if c == nil || c.http == nil {
		return nil, errors.New("BiniLyrics client is nil")
	}
	input := names.Normalize(song, artist, album)
	endpoint, err := url.Parse(c.baseURL + "/v2/lyrics/get")
	if err != nil {
		return nil, err
	}
	query := endpoint.Query()
	query.Set("title", input.TrackName)
	if input.ArtistName != "" {
		query.Set("artist", input.ArtistName)
	}
	if input.AlbumName != "" {
		query.Set("album", input.AlbumName)
	}
	if duration > 0 {
		query.Set("duration", strconv.FormatFloat(duration, 'f', -1, 64))
	}
	query.Set("source", "apple")
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, application/ttml+xml, text/plain")
	req.Header.Set("User-Agent", c.agent)
	if err := c.pace.Wait(ctx); err != nil {
		return nil, err
	}
	response, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("BiniLyrics returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	content := strings.TrimSpace(string(body))
	if strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "json") {
		content = extractContent(content)
	}
	if content == "" {
		return nil, ErrNotFound
	}
	return &Result{Content: content, SyncType: "syllable", Source: "binilyrics"}, nil
}

func extractContent(raw string) string {
	var value any
	if json.Unmarshal([]byte(raw), &value) != nil {
		return ""
	}
	return findText(value)
}

func findText(value any) string {
	switch current := value.(type) {
	case string:
		return current
	case []any:
		for _, item := range current {
			if found := findText(item); found != "" {
				return found
			}
		}
	case map[string]any:
		for _, key := range []string{"lyrics", "ttml", "content", "data", "result"} {
			if found := findText(current[key]); found != "" {
				return found
			}
		}
	}
	return ""
}
