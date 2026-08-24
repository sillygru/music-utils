package musixmatch

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

var ErrNotFound = errors.New("Musixmatch lyrics not found")

const maxResponseBytes = 8 << 20

type Result struct {
	PlainLyrics string
	Source      string
	Language    string
	Copyright   string
}

type Client struct {
	baseURL   string
	apiKey    string
	userAgent string
	http      *http.Client
	pace      *pacer.Pacer
}

func New(baseURL, apiKey, userAgent string, timeout time.Duration) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("invalid Musixmatch base URL")
	}
	if strings.TrimSpace(apiKey) == "" || strings.TrimSpace(userAgent) == "" || timeout <= 0 {
		return nil, fmt.Errorf("Musixmatch API key, user agent, and timeout are required")
	}
	return &Client{baseURL: baseURL, apiKey: strings.TrimSpace(apiKey), userAgent: userAgent, http: &http.Client{Timeout: timeout}, pace: pacer.New(200 * time.Millisecond)}, nil
}

func (c *Client) SearchTrack(ctx context.Context, trackName, artistName, albumName string, duration float64) (*Track, error) {
	input := names.Normalize(trackName, artistName, albumName)
	endpoint, err := url.Parse(c.baseURL + "/ws/1.1/track.search")
	if err != nil {
		return nil, err
	}
	q := endpoint.Query()
	q.Set("apikey", c.apiKey)
	q.Set("q_track", input.TrackName)
	q.Set("q_artist", input.ArtistName)
	q.Set("f_has_lyrics", "1")
	q.Set("page_size", "25")
	if input.AlbumName != "" {
		q.Set("q", strings.Join([]string{input.TrackName, input.ArtistName, input.AlbumName}, " "))
	}
	endpoint.RawQuery = q.Encode()
	var response searchResponse
	if err := c.doJSON(ctx, endpoint.String(), &response); err != nil {
		return nil, err
	}
	best := chooseTrack(response.Message.Body.TrackList, input, duration)
	if best == nil {
		return nil, ErrNotFound
	}
	return best, nil
}

func (c *Client) GetLyrics(ctx context.Context, trackID, isrc string) (*Result, error) {
	endpoint, err := url.Parse(c.baseURL + "/ws/1.1/track.lyrics.get")
	if err != nil {
		return nil, err
	}
	q := endpoint.Query()
	q.Set("apikey", c.apiKey)
	if strings.TrimSpace(isrc) != "" {
		q.Set("track_isrc", strings.TrimSpace(isrc))
	} else {
		q.Set("commontrack_id", strings.TrimSpace(trackID))
	}
	endpoint.RawQuery = q.Encode()
	var response lyricsResponse
	if err := c.doJSON(ctx, endpoint.String(), &response); err != nil {
		return nil, err
	}
	lyrics := strings.TrimSpace(response.Message.Body.Lyrics.LyricsBody)
	if lyrics == "" {
		return nil, ErrNotFound
	}
	return &Result{PlainLyrics: lyrics, Source: "musixmatch", Language: response.Message.Body.Lyrics.Language, Copyright: response.Message.Body.Lyrics.Copyright}, nil
}

func (c *Client) doJSON(ctx context.Context, endpoint string, target any) error {
	if c == nil || c.http == nil {
		return errors.New("Musixmatch client is nil")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if err := c.pace.Wait(ctx); err != nil {
		return err
	}
	response, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("request Musixmatch: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Musixmatch returned HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(target); err != nil {
		return fmt.Errorf("decode Musixmatch response: %w", err)
	}
	return nil
}

type Track struct {
	ID            string
	CommonTrackID string
	Name          string
	ArtistName    string
	AlbumName     string
	Duration      float64
	ISRC          string
}
type searchResponse struct {
	Message struct {
		Body struct {
			TrackList []struct {
				Track struct {
					TrackID       int64   `json:"track_id"`
					CommonTrackID int64   `json:"commontrack_id"`
					TrackName     string  `json:"track_name"`
					ArtistName    string  `json:"artist_name"`
					AlbumName     string  `json:"album_name"`
					TrackLength   float64 `json:"track_length"`
					ISRC          string  `json:"track_isrc"`
				} `json:"track"`
			} `json:"track_list"`
		} `json:"body"`
	} `json:"message"`
}
type lyricsResponse struct {
	Message struct {
		Body struct {
			Lyrics struct {
				LyricsBody string `json:"lyrics_body"`
				Language   string `json:"lyrics_language"`
				Copyright  string `json:"lyrics_copyright"`
			} `json:"lyrics"`
		} `json:"body"`
	} `json:"message"`
}

func chooseTrack(items []struct {
	Track struct {
		TrackID       int64   `json:"track_id"`
		CommonTrackID int64   `json:"commontrack_id"`
		TrackName     string  `json:"track_name"`
		ArtistName    string  `json:"artist_name"`
		AlbumName     string  `json:"album_name"`
		TrackLength   float64 `json:"track_length"`
		ISRC          string  `json:"track_isrc"`
	} `json:"track"`
}, input names.Input, duration float64) *Track {
	var best *Track
	bestScore := -1.0
	for _, item := range items {
		t := item.Track
		normalized := names.Normalize(t.TrackName, t.ArtistName, t.AlbumName)
		score := match(normalized.TrackName, input.TrackName)*3 + match(normalized.ArtistName, input.ArtistName)*3
		if input.AlbumName != "" {
			score += match(normalized.AlbumName, input.AlbumName)
		}
		if duration > 0 && t.TrackLength > 0 {
			d := t.TrackLength - duration
			if d < 0 {
				d = -d
			}
			score += 1 / (1 + d)
		}
		if score > bestScore {
			bestScore = score
			best = &Track{ID: strconv.FormatInt(t.TrackID, 10), CommonTrackID: strconv.FormatInt(t.CommonTrackID, 10), Name: t.TrackName, ArtistName: t.ArtistName, AlbumName: t.AlbumName, Duration: t.TrackLength, ISRC: t.ISRC}
		}
	}
	return best
}
func match(a, b string) float64 {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	if a == b && a != "" {
		return 1
	}
	if a != "" && b != "" && (strings.Contains(a, b) || strings.Contains(b, a)) {
		return .75
	}
	return 0
}
