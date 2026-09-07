package betterlyrics

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
	"github.com/sillygru/music-utils/internal/ttml"
)

// ErrNotFound reports that BetterLyrics has no lyrics for the track.
var ErrNotFound = errors.New("betterlyrics lyrics not found")

const maxResponseBytes = 4 << 20

// Result is one BetterLyrics lookup outcome.
type Result struct {
	PlainLyrics  string
	SyncedLyrics string
	TTML         string
	WordSynced   bool
}

// Client fetches TTML lyrics from a BetterLyrics-compatible API.
type Client struct {
	baseURL   string
	userAgent string
	http      *http.Client
	pace      *pacer.Pacer
}

// New creates a client for baseURL (e.g. https://lyrics-api.boidu.dev).
func New(baseURL, userAgent string, timeout time.Duration) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("invalid BetterLyrics base URL")
	}
	if strings.TrimSpace(userAgent) == "" || timeout <= 0 {
		return nil, fmt.Errorf("BetterLyrics user agent and timeout are required")
	}
	return &Client{
		baseURL:   baseURL,
		userAgent: userAgent,
		http:      &http.Client{Timeout: timeout},
		pace:      pacer.New(200 * time.Millisecond),
	}, nil
}

// Get fetches TTML for a song and converts it to LRC. Duration is seconds;
// it is forwarded in milliseconds only when positive, matching the upstream
// contract. Matching is exact on title/artist upstream, so the input is sent
// un-normalized apart from trimming.
func (c *Client) Get(ctx context.Context, trackName, artistName, albumName string, duration float64) (*Result, error) {
	if c == nil || c.http == nil {
		return nil, errors.New("BetterLyrics client is nil")
	}
	trackName, artistName, albumName = strings.TrimSpace(trackName), strings.TrimSpace(artistName), strings.TrimSpace(albumName)
	if trackName == "" || artistName == "" {
		return nil, ErrNotFound
	}
	_ = names.Normalize // keep import if unused in future; inputs stay exact
	endpoint, err := url.Parse(c.baseURL + "/getLyrics")
	if err != nil {
		return nil, fmt.Errorf("build BetterLyrics URL: %w", err)
	}
	query := endpoint.Query()
	query.Set("s", trackName)
	query.Set("a", artistName)
	if duration > 0 {
		query.Set("d", strconv.FormatInt(int64(duration*1000), 10))
	}
	if albumName != "" {
		query.Set("al", albumName)
	}
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create BetterLyrics request: %w", err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")
	if err := c.pace.Wait(ctx); err != nil {
		return nil, err
	}
	response, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request BetterLyrics: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("BetterLyrics returned HTTP %d", response.StatusCode)
	}
	var payload struct {
		TTML string `json:"ttml"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode BetterLyrics response: %w", err)
	}
	if strings.TrimSpace(payload.TTML) == "" {
		return nil, ErrNotFound
	}
	synced, err := ttml.ParseToLRC(payload.TTML)
	if err != nil || strings.TrimSpace(synced) == "" {
		return nil, ErrNotFound
	}
	// Detect word-level timing for rich JSON path.
	wordSynced := false
	if lines, parseErr := ttml.Parse(payload.TTML); parseErr == nil {
		for _, line := range lines {
			if len(line.Words) > 0 {
				wordSynced = true
				break
			}
		}
	}
	plain := ttml.PlainText(payload.TTML)
	if plain == "" {
		plain = ttml.ExtractPlainFromLRC(synced)
	}
	synced = ttml.CleanSyncedLyrics(synced)
	return &Result{PlainLyrics: plain, SyncedLyrics: synced, TTML: payload.TTML, WordSynced: wordSynced}, nil
}
