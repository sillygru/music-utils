package lrclib

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

var ErrNotFound = errors.New("lrclib track not found")

const (
	maxResponseBytes = 2 << 20
	maxIdleConns     = 100
	// requestInterval paces LRCLIB to five requests per second process-wide.
	// LRCLIB is community-run with no documented quota; a gentle shared rate
	// keeps the server's IP welcome regardless of client traffic.
	requestInterval = time.Second / 5
)

// RemoteResult is the response shape returned by LRCLIB search and exact
// lookup. ID and Name are populated by search; exact lookup may omit them.
type RemoteResult struct {
	ID           int64   `json:"id"`
	Name         string  `json:"name,omitempty"`
	TrackName    string  `json:"trackName"`
	ArtistName   string  `json:"artistName"`
	AlbumName    string  `json:"albumName"`
	Duration     float64 `json:"duration"`
	Instrumental bool    `json:"instrumental"`
	PlainLyrics  string  `json:"plainLyrics"`
	SyncedLyrics string  `json:"syncedLyrics"`
}

// Client retrieves lyrics from LRCLIB.
type Client struct {
	baseURL   string
	userAgent string
	http      *http.Client
	pace      *pacer.Pacer
}

// New creates a client for baseURL, which should point at LRCLIB's /api path.
func New(baseURL, userAgent string, timeout time.Duration) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("invalid LRCLIB base URL")
	}
	if strings.TrimSpace(userAgent) == "" {
		return nil, fmt.Errorf("LRCLIB user agent is empty")
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("LRCLIB timeout must be positive")
	}
	var transport http.RoundTripper = http.DefaultTransport
	if defaultTransport, ok := http.DefaultTransport.(*http.Transport); ok {
		optimizedTransport := defaultTransport.Clone()
		optimizedTransport.MaxIdleConns = maxIdleConns
		optimizedTransport.MaxIdleConnsPerHost = maxIdleConns
		optimizedTransport.IdleConnTimeout = 90 * time.Second
		transport = optimizedTransport
	}

	return &Client{
		baseURL:   baseURL,
		userAgent: userAgent,
		http:      &http.Client{Timeout: timeout, Transport: transport},
		pace:      pacer.New(requestInterval),
	}, nil
}

// Search performs a text search and returns the array LRCLIB provides.
func (c *Client) Search(ctx context.Context, query string) ([]RemoteResult, error) {
	query = names.CleanSearch(query)
	if c == nil || c.http == nil {
		return nil, errors.New("LRCLIB client is nil")
	}
	endpoint, err := url.Parse(c.baseURL + "/search")
	if err != nil {
		return nil, fmt.Errorf("build LRCLIB URL: %w", err)
	}
	params := endpoint.Query()
	params.Set("q", strings.TrimSpace(query))
	endpoint.RawQuery = params.Encode()
	var results []RemoteResult
	if err := c.doJSON(ctx, endpoint.String(), &results); err != nil {
		return nil, err
	}
	return results, nil
}

// GetExact performs one request and returns ErrNotFound for a remote 404.
func (c *Client) GetExact(ctx context.Context, trackName, artistName, albumName string, duration float64) (*RemoteResult, error) {
	input := names.Normalize(trackName, artistName, albumName)
	trackName, artistName, albumName = input.TrackName, input.ArtistName, input.AlbumName
	if c == nil || c.http == nil {
		return nil, errors.New("LRCLIB client is nil")
	}
	endpoint, err := url.Parse(c.baseURL + "/get")
	if err != nil {
		return nil, fmt.Errorf("build LRCLIB URL: %w", err)
	}
	query := endpoint.Query()
	query.Set("track_name", trackName)
	query.Set("artist_name", artistName)
	if strings.TrimSpace(albumName) != "" {
		query.Set("album_name", albumName)
	}
	if duration > 0 {
		query.Set("duration", strconv.FormatFloat(duration, 'f', -1, 64))
	}
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create LRCLIB request: %w", err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")

	var result RemoteResult
	if err := c.doJSON(ctx, endpoint.String(), &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *Client) doJSON(ctx context.Context, endpoint string, value any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("create LRCLIB request: %w", err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")
	if err := c.pace.Wait(ctx); err != nil {
		return err
	}
	response, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("request LRCLIB: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("LRCLIB returned HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(value); err != nil {
		return fmt.Errorf("decode LRCLIB response: %w", err)
	}
	return nil
}
