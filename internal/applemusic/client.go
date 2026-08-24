package applemusic

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

var ErrNotFound = errors.New("apple music lyrics not found")

const maxResponseBytes = 8 << 20

type Result struct {
	Content  string
	Format   string
	SyncType string
	Source   string
}

type Client struct {
	catalogBaseURL string
	lyricsBaseURL  string
	storefront     string
	userAgent      string
	mediaTokens    []string
	http           *http.Client
	pace           *pacer.Pacer
}

func New(catalogBaseURL, lyricsBaseURL, storefront, userAgent string, mediaTokens []string, timeout time.Duration) (*Client, error) {
	catalogBaseURL = strings.TrimRight(strings.TrimSpace(catalogBaseURL), "/")
	lyricsBaseURL = strings.TrimRight(strings.TrimSpace(lyricsBaseURL), "/")
	for _, raw := range []string{catalogBaseURL, lyricsBaseURL} {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return nil, fmt.Errorf("invalid Apple Music URL")
		}
	}
	if strings.TrimSpace(userAgent) == "" || timeout <= 0 {
		return nil, fmt.Errorf("Apple Music user agent and timeout are required")
	}
	if strings.TrimSpace(storefront) == "" {
		storefront = "us"
	}
	return &Client{
		catalogBaseURL: catalogBaseURL,
		lyricsBaseURL:  lyricsBaseURL,
		storefront:     strings.ToLower(strings.TrimSpace(storefront)),
		userAgent:      userAgent,
		mediaTokens:    cleanTokens(mediaTokens),
		http:           &http.Client{Timeout: timeout},
		pace:           pacer.New(500 * time.Millisecond),
	}, nil
}

// SearchTrack resolves a track through the Apple Music catalog. The catalog
// path and response shape are compatible with Apple Music's public catalog
// search API; a developer token is required when the configured endpoint needs
// authentication.
func (c *Client) SearchTrack(ctx context.Context, trackName, artistName, albumName string, duration float64) (*Track, error) {
	if c == nil || c.http == nil {
		return nil, errors.New("Apple Music client is nil")
	}
	input := names.Normalize(trackName, artistName, albumName)
	endpoint, err := url.Parse(c.catalogBaseURL + "/v1/catalog/" + url.PathEscape(c.storefront) + "/search")
	if err != nil {
		return nil, fmt.Errorf("build Apple Music search URL: %w", err)
	}
	query := endpoint.Query()
	query.Set("term", strings.Join(nonEmpty(input.TrackName, input.ArtistName), " "))
	query.Set("types", "songs")
	query.Set("limit", "25")
	endpoint.RawQuery = query.Encode()
	var response catalogResponse
	if err := c.doJSON(ctx, endpoint.String(), &response); err != nil {
		return nil, err
	}
	best := chooseTrack(response.Results.Songs.Data, input, duration)
	if best == nil {
		return nil, ErrNotFound
	}
	return best, nil
}

// GetLyrics retrieves TTML for an Apple Music track ID. The default path is
// intentionally configurable because Apple Music's web lyrics endpoint is not
// a stable public API and deployments may use a compliant proxy.
func (c *Client) GetLyrics(ctx context.Context, trackID string) (*Result, error) {
	trackID = strings.TrimSpace(trackID)
	if trackID == "" {
		return nil, ErrNotFound
	}
	endpoint, err := url.Parse(c.lyricsBaseURL + "/v1/catalog/" + url.PathEscape(c.storefront) + "/songs/" + url.PathEscape(trackID) + "/lyrics")
	if err != nil {
		return nil, fmt.Errorf("build Apple Music lyrics URL: %w", err)
	}
	var response lyricsResponse
	if err := c.doJSON(ctx, endpoint.String(), &response); err != nil {
		return nil, err
	}
	for _, item := range response.Data {
		if strings.TrimSpace(item.Attributes.TTML) != "" {
			return &Result{Content: item.Attributes.TTML, Format: "ttml", SyncType: "richsync", Source: "apple_music"}, nil
		}
	}
	return nil, ErrNotFound
}

func (c *Client) doJSON(ctx context.Context, endpoint string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if len(c.mediaTokens) > 0 {
		req.Header.Set("Media-User-Token", c.mediaTokens[0])
	}
	if err := c.pace.Wait(ctx); err != nil {
		return err
	}
	response, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("request Apple Music: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Apple Music returned HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(target); err != nil {
		return fmt.Errorf("decode Apple Music response: %w", err)
	}
	return nil
}

type Track struct {
	ID         string
	Name       string
	ArtistName string
	AlbumName  string
	Duration   float64
	ISRC       string
}

type catalogResponse struct {
	Results struct {
		Songs struct {
			Data []struct {
				ID         string `json:"id"`
				Attributes struct {
					Name       string  `json:"name"`
					ArtistName string  `json:"artistName"`
					AlbumName  string  `json:"albumName"`
					DurationMS float64 `json:"durationInMillis"`
					ISRC       string  `json:"isrc"`
				} `json:"attributes"`
			} `json:"data"`
		} `json:"songs"`
	} `json:"results"`
}

type lyricsResponse struct {
	Data []struct {
		Attributes struct {
			TTML string `json:"ttml"`
		} `json:"attributes"`
	} `json:"data"`
}

func chooseTrack(items []struct {
	ID         string `json:"id"`
	Attributes struct {
		Name       string  `json:"name"`
		ArtistName string  `json:"artistName"`
		AlbumName  string  `json:"albumName"`
		DurationMS float64 `json:"durationInMillis"`
		ISRC       string  `json:"isrc"`
	} `json:"attributes"`
}, input names.Input, duration float64) *Track {
	var best *Track
	bestScore := -1.0
	for _, item := range items {
		candidate := names.Normalize(item.Attributes.Name, item.Attributes.ArtistName, item.Attributes.AlbumName)
		score := similarityScore(candidate.TrackName, input.TrackName)*3 + similarityScore(candidate.ArtistName, input.ArtistName)*3
		if input.AlbumName != "" {
			score += similarityScore(candidate.AlbumName, input.AlbumName)
		}
		if duration > 0 && item.Attributes.DurationMS > 0 {
			diff := item.Attributes.DurationMS/1000 - duration
			if diff < 0 {
				diff = -diff
			}
			score += 1 / (1 + diff)
		}
		if score > bestScore {
			bestScore = score
			best = &Track{ID: item.ID, Name: item.Attributes.Name, ArtistName: item.Attributes.ArtistName, AlbumName: item.Attributes.AlbumName, Duration: item.Attributes.DurationMS / 1000, ISRC: item.Attributes.ISRC}
		}
	}
	return best
}

func similarityScore(a, b string) float64 {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1
	}
	if strings.Contains(a, b) || strings.Contains(b, a) {
		return .75
	}
	return 0
}

func cleanTokens(tokens []string) []string {
	out := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if token = strings.TrimSpace(token); token != "" {
			out = append(out, token)
		}
	}
	return out
}

func nonEmpty(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}
