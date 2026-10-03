package richlyrics

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
	"github.com/sillygru/music-utils/internal/upstream"
)

var ErrNotFound = errors.New("rich lyrics not found")

const (
	maxResponseBytes = 4 << 20
)

// RequestInterval is how far apart two Unison requests are spaced when the caller
// does not supply its own pacer.
//
// It is exported because the server has to state the same rate when it applies
// UPSTREAM_PACE_MS to this client. Unison sits on no shared lease, because the
// lyrics backfill never asks it for lyrics, so the number the client paces itself
// at and the number the server hands it have to come from the same place.
const RequestInterval = time.Second / 5

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
	pace      pacer.Waiter
}

func New(baseURL, userAgent string, timeout time.Duration) (*Client, error) {
	return NewWithPacer(baseURL, userAgent, timeout, nil)
}

// NewWithPacer creates a client that spaces its requests using pace, falling
// back to the provider's own interval when pace is nil. A batch job passes a
// pacer shared with the live server so a backfill yields to real traffic
// instead of competing with it for the same upstream budget.
func NewWithPacer(baseURL, userAgent string, timeout time.Duration, pace pacer.Waiter) (*Client, error) {
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
		pace:      pacer.OrDefault(pace, RequestInterval),
	}, nil
}

// Get resolves a rich/syllable lyrics payload by song metadata. Only title,
// artist, and album are ever sent: duration and all other metadata are
// deliberately excluded so a duration mismatch can never filter out the
// correct recording upstream. Unison uses song/artist/album parameter names;
// the same shape is accepted by compatible mirrors.
func (c *Client) Get(ctx context.Context, trackName, artistName, albumName string) (*Result, error) {
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
	if err := upstream.CheckStatus("rich lyrics", response); err != nil {
		return nil, err
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
	Success bool        `json:"success"`
	Data    *unisonData `json:"data"`
	Lyrics  string      `json:"lyrics"`
	Format  string      `json:"format"`
	Sync    string      `json:"syncType"`
	Type    string      `json:"sync_type"`
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
