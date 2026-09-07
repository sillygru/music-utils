package lrclib

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

// SearchFiltered searches with typed track/artist/album parameters and picks
// the closest match. It backs the multi-strategy exact lookup below.
func (c *Client) SearchFiltered(ctx context.Context, trackName, artistName, albumName string) ([]RemoteResult, error) {
	if c == nil || c.http == nil {
		return nil, errors.New("LRCLIB client is nil")
	}
	endpoint, err := url.Parse(c.baseURL + "/search")
	if err != nil {
		return nil, fmt.Errorf("build LRCLIB URL: %w", err)
	}
	params := endpoint.Query()
	if strings.TrimSpace(trackName) != "" {
		params.Set("track_name", strings.TrimSpace(trackName))
	}
	if strings.TrimSpace(artistName) != "" {
		params.Set("artist_name", strings.TrimSpace(artistName))
	}
	if strings.TrimSpace(albumName) != "" {
		params.Set("album_name", strings.TrimSpace(albumName))
	}
	endpoint.RawQuery = params.Encode()
	var results []RemoteResult
	if err := c.doJSON(ctx, endpoint.String(), &results); err != nil {
		return nil, err
	}
	return results, nil
}

// GetWithFallbacks runs the multi-strategy exact lookup: typed
// track+artist+album, track only, free-text "artist title", title only, then
// the original un-cleaned names. Duration (seconds, <=0 unknown) is never
// sent upstream; it only ranks candidates locally (±2s strict, ±5s relaxed).
func (c *Client) GetWithFallbacks(ctx context.Context, trackName, artistName, albumName string, duration float64) (*RemoteResult, error) {
	input := names.Normalize(trackName, artistName, albumName)
	type strategy struct {
		track, artist, album string
		freeText             string
	}
	strategies := []strategy{
		{track: input.TrackName, artist: input.ArtistName, album: input.AlbumName},
		{track: input.TrackName},
		{freeText: strings.TrimSpace(input.ArtistName + " " + input.TrackName)},
		{freeText: input.TrackName},
		{track: strings.TrimSpace(trackName), artist: strings.TrimSpace(artistName)},
	}
	var relaxed *RemoteResult
	for _, item := range strategies {
		var results []RemoteResult
		var err error
		if item.freeText != "" {
			results, err = c.Search(ctx, item.freeText)
		} else {
			results, err = c.SearchFiltered(ctx, item.track, item.artist, item.album)
		}
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, err
		}
		if match := bestMatch(results, input.TrackName, input.ArtistName, duration, 2); match != nil {
			return match, nil
		}
		if relaxed == nil {
			relaxed = bestMatch(results, input.TrackName, input.ArtistName, duration, 5)
		}
	}
	if relaxed != nil {
		return relaxed, nil
	}
	return nil, ErrNotFound
}

// bestMatch picks the first result whose track/artist names match and whose
// duration falls within tolerance seconds of the hint (when both are known).
func bestMatch(results []RemoteResult, trackName, artistName string, duration, tolerance float64) *RemoteResult {
	for i := range results {
		result := &results[i]
		if !nameMatches(result.TrackName, trackName) {
			continue
		}
		if strings.TrimSpace(artistName) != "" && !nameMatches(result.ArtistName, artistName) {
			continue
		}
		if result.PlainLyrics == "" && result.SyncedLyrics == "" && !result.Instrumental {
			continue
		}
		if duration > 0 && result.Duration > 0 && absDuration(result.Duration-duration) > tolerance {
			continue
		}
		return result
	}
	return nil
}

func nameMatches(candidate, want string) bool {
	candidate = strings.ToLower(strings.TrimSpace(candidate))
	want = strings.ToLower(strings.TrimSpace(want))
	if candidate == "" || want == "" {
		return false
	}
	if candidate == want {
		return true
	}
	return strings.Contains(candidate, want) || strings.Contains(want, candidate)
}

func absDuration(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}

// GetExact performs one request and returns ErrNotFound for a remote 404.
// Only title, artist, and album are ever sent: duration and all other
// metadata are deliberately excluded so a duration mismatch can never filter
// out the correct recording upstream.
func (c *Client) GetExact(ctx context.Context, trackName, artistName, albumName string) (*RemoteResult, error) {
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
