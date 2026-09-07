package kugou

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sillygru/music-utils/internal/names"
	"github.com/sillygru/music-utils/internal/pacer"
)

// ErrNotFound reports that KuGou has no lyrics for the track.
var ErrNotFound = errors.New("kugou lyrics not found")

const (
	maxResponseBytes = 4 << 20
	// durationTolerance is the maximum song-duration mismatch accepted when
	// matching KuGou candidates, mirroring Metrolist's 8s tolerance.
	durationTolerance = 8.0
)

// Result is one KuGou lookup outcome.
type Result struct {
	TrackName    string
	ArtistName   string
	AlbumName    string
	Duration     float64
	PlainLyrics  string
	SyncedLyrics string
}

// Client fetches lyrics through KuGou's search + download pipeline.
type Client struct {
	searchBaseURL string
	lyricsBaseURL string
	userAgent     string
	http          *http.Client
	pace          *pacer.Pacer
}

// New creates a client. searchBaseURL is the mobileservice host
// (https://mobileservice.kugou.com), lyricsBaseURL the lyrics host
// (https://lyrics.kugou.com).
func New(searchBaseURL, lyricsBaseURL, userAgent string, timeout time.Duration) (*Client, error) {
	searchBaseURL = strings.TrimRight(strings.TrimSpace(searchBaseURL), "/")
	lyricsBaseURL = strings.TrimRight(strings.TrimSpace(lyricsBaseURL), "/")
	for _, raw := range []string{searchBaseURL, lyricsBaseURL} {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return nil, fmt.Errorf("invalid KuGou URL")
		}
	}
	if strings.TrimSpace(userAgent) == "" || timeout <= 0 {
		return nil, fmt.Errorf("KuGou user agent and timeout are required")
	}
	return &Client{
		searchBaseURL: searchBaseURL,
		lyricsBaseURL: lyricsBaseURL,
		userAgent:     userAgent,
		http:          &http.Client{Timeout: timeout},
		pace:          pacer.New(500 * time.Millisecond),
	}, nil
}

// Get resolves lyrics by title/artist/album. Duration is seconds; negative or
// zero means unknown and disables duration filtering.
func (c *Client) Get(ctx context.Context, trackName, artistName, albumName string, duration float64) (*Result, error) {
	if c == nil || c.http == nil {
		return nil, errors.New("KuGou client is nil")
	}
	input := names.Normalize(trackName, artistName, albumName)
	keyword := songKeyword(input.TrackName, input.ArtistName, input.AlbumName)
	if keyword == "" {
		return nil, ErrNotFound
	}
	// 1. Song search by keyword, then lyrics candidates by hash.
	if songs, err := c.searchSongs(ctx, keyword); err == nil {
		for _, song := range songs {
			if duration > 0 && song.Duration > 0 && abs(song.Duration-duration) > durationTolerance {
				continue
			}
			candidates, err := c.searchLyricsByHash(ctx, song.Hash)
			if err != nil {
				continue
			}
			if result := c.tryCandidates(ctx, candidates, duration); result != nil {
				return result, nil
			}
		}
	}
	// 2. Keyword fallback directly against the lyrics search.
	candidates, err := c.searchLyricsByKeyword(ctx, keyword, duration)
	if err != nil {
		return nil, ErrNotFound
	}
	if result := c.tryCandidates(ctx, candidates, duration); result != nil {
		return result, nil
	}
	return nil, ErrNotFound
}

func (c *Client) tryCandidates(ctx context.Context, candidates []candidate, duration float64) *Result {
	for _, cand := range candidates {
		if duration > 0 && cand.Duration > 0 && abs(cand.Duration/1000-duration) > durationTolerance {
			continue
		}
		lrc, err := c.download(ctx, cand.ID, cand.AccessKey)
		if err != nil || strings.TrimSpace(lrc) == "" {
			continue
		}
		plain := StripTimestamps(lrc)
		return &Result{PlainLyrics: plain, SyncedLyrics: lrc}
	}
	return nil
}

type songInfo struct {
	Duration float64
	Hash     string
}

func (c *Client) searchSongs(ctx context.Context, keyword string) ([]songInfo, error) {
	endpoint, err := url.Parse(c.searchBaseURL + "/api/v3/search/song")
	if err != nil {
		return nil, err
	}
	query := endpoint.Query()
	query.Set("version", "9108")
	query.Set("plat", "0")
	query.Set("pagesize", "8")
	query.Set("showtype", "0")
	query.Set("keyword", keyword)
	endpoint.RawQuery = query.Encode()
	var payload struct {
		Status int `json:"status"`
		Data   struct {
			Info []struct {
				Duration float64 `json:"duration"`
				Hash     string  `json:"hash"`
			} `json:"info"`
		} `json:"data"`
	}
	if err := c.doJSON(ctx, endpoint.String(), &payload); err != nil {
		return nil, err
	}
	out := make([]songInfo, 0, len(payload.Data.Info))
	for _, info := range payload.Data.Info {
		if strings.TrimSpace(info.Hash) == "" {
			continue
		}
		out = append(out, songInfo{Duration: info.Duration, Hash: info.Hash})
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out, nil
}

type candidate struct {
	ID        int64
	Duration  float64
	AccessKey string
}

func (c *Client) searchLyricsByHash(ctx context.Context, hash string) ([]candidate, error) {
	endpoint, err := url.Parse(c.lyricsBaseURL + "/search")
	if err != nil {
		return nil, err
	}
	query := endpoint.Query()
	query.Set("ver", "1")
	query.Set("man", "yes")
	query.Set("client", "pc")
	query.Set("hash", hash)
	endpoint.RawQuery = query.Encode()
	return c.decodeCandidates(ctx, endpoint.String())
}

func (c *Client) searchLyricsByKeyword(ctx context.Context, keyword string, duration float64) ([]candidate, error) {
	endpoint, err := url.Parse(c.lyricsBaseURL + "/search")
	if err != nil {
		return nil, err
	}
	query := endpoint.Query()
	query.Set("ver", "1")
	query.Set("man", "yes")
	query.Set("client", "pc")
	if duration > 0 {
		query.Set("duration", strconv.FormatInt(int64(duration*1000), 10))
	}
	query.Set("keyword", keyword)
	endpoint.RawQuery = query.Encode()
	return c.decodeCandidates(ctx, endpoint.String())
}

func (c *Client) decodeCandidates(ctx context.Context, endpoint string) ([]candidate, error) {
	var payload struct {
		Candidates []struct {
			ID        int64   `json:"id"`
			Duration  float64 `json:"duration"`
			AccessKey string  `json:"accesskey"`
		} `json:"candidates"`
	}
	if err := c.doJSON(ctx, endpoint, &payload); err != nil {
		return nil, err
	}
	out := make([]candidate, 0, len(payload.Candidates))
	for _, cand := range payload.Candidates {
		if cand.ID == 0 || strings.TrimSpace(cand.AccessKey) == "" {
			continue
		}
		out = append(out, candidate{ID: cand.ID, Duration: cand.Duration, AccessKey: cand.AccessKey})
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out, nil
}

func (c *Client) download(ctx context.Context, id int64, accessKey string) (string, error) {
	endpoint, err := url.Parse(c.lyricsBaseURL + "/download")
	if err != nil {
		return "", err
	}
	query := endpoint.Query()
	query.Set("fmt", "lrc")
	query.Set("charset", "utf8")
	query.Set("client", "pc")
	query.Set("ver", "1")
	query.Set("id", strconv.FormatInt(id, 10))
	query.Set("accesskey", accessKey)
	endpoint.RawQuery = query.Encode()
	var payload struct {
		Content string `json:"content"`
	}
	if err := c.doJSON(ctx, endpoint.String(), &payload); err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(payload.Content))
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(strings.TrimSpace(payload.Content))
		if err != nil {
			return "", fmt.Errorf("decode KuGou lyrics: %w", err)
		}
	}
	return Normalize(string(raw)), nil
}

func (c *Client) doJSON(ctx context.Context, endpoint string, target any) error {
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
		return fmt.Errorf("request KuGou: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("KuGou returned HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(target); err != nil {
		return fmt.Errorf("decode KuGou response: %w", err)
	}
	return nil
}

var (
	timedLinePattern = regexp.MustCompile(`\[(\d{1,3}):(\d\d)\.(\d{2,3})\]`)
	metadataPattern  = regexp.MustCompile(`^.+\].*[：:].+$`)
)

// Normalize filters decoded KuGou content down to timed LRC lines and trims
// leading/trailing metadata lines (singer/writer/composer credits).
func Normalize(content string) string {
	lines := strings.Split(content, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if !timedLinePattern.MatchString(line) {
			continue
		}
		kept = append(kept, strings.TrimSpace(line))
	}
	for len(kept) > 0 && metadataPattern.MatchString(kept[0]) {
		kept = kept[1:]
	}
	for len(kept) > 0 && metadataPattern.MatchString(kept[len(kept)-1]) {
		kept = kept[:len(kept)-1]
	}
	return strings.Join(kept, "\n")
}

// StripTimestamps returns the plain text of LRC content.
func StripTimestamps(lrc string) string {
	var b strings.Builder
	for _, line := range strings.Split(lrc, "\n") {
		text := strings.TrimSpace(stripTimestamp(line))
		if text == "" {
			continue
		}
		b.WriteString(text)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

func stripTimestamp(line string) string {
	for {
		loc := timedLinePattern.FindStringIndex(line)
		if loc == nil || loc[0] != 0 {
			return strings.TrimSpace(line)
		}
		line = line[loc[1]:]
	}
}

func songKeyword(track, artist, album string) string {
	parts := track
	if artist != "" {
		parts = track + " - " + artist
	}
	if album != "" {
		parts += " " + album
	}
	return strings.TrimSpace(parts)
}

func abs(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}
