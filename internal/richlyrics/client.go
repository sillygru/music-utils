package richlyrics

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

var ErrNotFound = errors.New("rich lyrics not found")

const (
	maxResponseBytes = 4 << 20
	requestInterval  = time.Second / 5
)

// Result is a source-native rich lyrics payload. Content is intentionally
// preserved rather than converted so TTML/QRC timing and annotations survive.
type Result struct {
	Content  string
	Format   string
	SyncType string
	Source   string
}

// Client reads the public Unison-compatible lyrics endpoint.
type Client struct {
	baseURL   string
	userAgent string
	http      *http.Client
	pace      *pacer.Pacer
}

func New(baseURL, userAgent string, timeout time.Duration) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("invalid rich lyrics base URL")
	}
	if strings.TrimSpace(userAgent) == "" {
		return nil, fmt.Errorf("rich lyrics user agent is empty")
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("rich lyrics timeout must be positive")
	}
	return &Client{
		baseURL:   baseURL,
		userAgent: userAgent,
		http:      &http.Client{Timeout: timeout},
		pace:      pacer.New(requestInterval),
	}, nil
}

// Get resolves a rich/syllable lyrics payload by song metadata. Unison uses
// song/artist/album/duration parameter names; the same shape is accepted by
// compatible mirrors.
func (c *Client) Get(ctx context.Context, trackName, artistName, albumName string, duration float64) (*Result, error) {
	if c == nil || c.http == nil {
		return nil, errors.New("rich lyrics client is nil")
	}
	input := names.Normalize(trackName, artistName, albumName)
	endpoint, err := url.Parse(c.baseURL + "/lyrics")
	if err != nil {
		return nil, fmt.Errorf("build rich lyrics URL: %w", err)
	}
	query := endpoint.Query()
	query.Set("song", input.TrackName)
	if input.ArtistName != "" {
		query.Set("artist", input.ArtistName)
	}
	if input.AlbumName != "" {
		query.Set("album", input.AlbumName)
	}
	if duration > 0 {
		query.Set("duration", strconv.FormatFloat(duration, 'f', -1, 64))
	}
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create rich lyrics request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if err := c.pace.Wait(ctx); err != nil {
		return nil, err
	}
	response, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request rich lyrics: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("rich lyrics returned HTTP %d", response.StatusCode)
	}

	var payload unisonResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode rich lyrics response: %w", err)
	}
	result := payload.result()
	if result == nil || strings.TrimSpace(result.Content) == "" {
		return nil, ErrNotFound
	}
	result.Source = "unison"
	if result.Format == "" {
		result.Format = "ttml"
	}
	if result.SyncType == "" {
		result.SyncType = "richsync"
	}
	return result, nil
}

type unisonResponse struct {
	Success bool             `json:"success"`
	Data    *unisonData      `json:"data"`
	Lyrics  string           `json:"lyrics"`
	Format  string           `json:"format"`
	Sync    string           `json:"syncType"`
	Type    string           `json:"sync_type"`
}

type unisonData struct {
	Lyrics   string `json:"lyrics"`
	Format   string `json:"format"`
	SyncType string `json:"syncType"`
	Sync     string `json:"sync_type"`
}

func (p unisonResponse) result() *Result {
	if p.Data != nil {
		return &Result{Content: p.Data.Lyrics, Format: p.Data.Format, SyncType: firstNonEmpty(p.Data.SyncType, p.Data.Sync)}
	}
	return &Result{Content: p.Lyrics, Format: p.Format, SyncType: firstNonEmpty(p.Sync, p.Type)}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
