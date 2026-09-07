package zemer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/sillygru/music-utils/internal/pacer"
)

// ErrNotFound reports that Zemer has no lyrics for the video.
var ErrNotFound = errors.New("zemer lyrics not found")

const maxResponseBytes = 8 << 20

// Result is one Zemer lookup outcome.
type Result struct {
	PlainLyrics  string
	SyncedLyrics string
	Instrumental bool
}

// Source is one lyrics pointer returned by the resolve endpoint.
type Source struct {
	Type      string `json:"type"`
	URL       string `json:"url"`
	SongID    int64  `json:"songId"`
	FeedURL   string `json:"feedUrl"`
	TrackID   int64  `json:"trackId"`
	Plain     string `json:"plain"`
	SyncedLRC string `json:"syncedLrc"`
	Synced    bool   `json:"synced"`
}

// Client resolves lyrics by YouTube videoId through search.zemer.io.
type Client struct {
	baseURL       string
	lrclibBaseURL string
	zingGraphQL   string
	userAgent     string
	http          *http.Client
	pace          *pacer.Pacer
}

// New creates a client for baseURL (https://search.zemer.io).
func New(baseURL, userAgent string, timeout time.Duration) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("invalid Zemer base URL")
	}
	if strings.TrimSpace(userAgent) == "" || timeout <= 0 {
		return nil, fmt.Errorf("Zemer user agent and timeout are required")
	}
	return &Client{
		baseURL:       baseURL,
		lrclibBaseURL: "https://lrclib.net/api",
		zingGraphQL:   "https://jewishmusic.fm:8443/graphql",
		userAgent:     userAgent,
		http:          &http.Client{Timeout: timeout},
		pace:          pacer.New(300 * time.Millisecond),
	}, nil
}

// Get resolves and fetches lyrics for one YouTube videoId.
func (c *Client) Get(ctx context.Context, videoID string) (*Result, error) {
	if c == nil || c.http == nil {
		return nil, errors.New("Zemer client is nil")
	}
	videoID = strings.TrimSpace(videoID)
	if videoID == "" {
		return nil, ErrNotFound
	}
	sources, err := c.resolve(ctx, videoID)
	if err != nil {
		return nil, err
	}
	for _, source := range rankSources(sources) {
		body, synced, err := c.fetchSource(ctx, source)
		if err != nil || !hasBody(body) {
			continue
		}
		return &Result{PlainLyrics: body, SyncedLyrics: synced}, nil
	}
	return nil, ErrNotFound
}

func (c *Client) resolve(ctx context.Context, videoID string) ([]Source, error) {
	endpoint, err := url.Parse(c.baseURL + "/lyrics/resolve")
	if err != nil {
		return nil, err
	}
	query := endpoint.Query()
	query.Set("videoId", videoID)
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if err := c.pace.Wait(ctx); err != nil {
		return nil, err
	}
	response, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request Zemer: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("Zemer returned HTTP %d", response.StatusCode)
	}
	var payload struct {
		Sources []Source `json:"sources"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode Zemer response: %w", err)
	}
	if len(payload.Sources) == 0 {
		return nil, ErrNotFound
	}
	return payload.Sources, nil
}

func sourceRank(sourceType string) int {
	switch strings.ToLower(strings.TrimSpace(sourceType)) {
	case "jkaraoke":
		return 0
	case "lrclib", "jyrics", "shironet", "zingmusic", "tab4u", "zemirotdb":
		return 1
	case "booklet", "manual":
		return 2
	case "canonical", "community":
		return 3
	default:
		return 9
	}
}

// rankSources tries synced sources before unsynced ones, then by type rank.
func rankSources(sources []Source) []Source {
	ordered := append([]Source(nil), sources...)
	less := func(a, b Source) bool {
		if a.Synced != b.Synced {
			return a.Synced
		}
		return sourceRank(a.Type) < sourceRank(b.Type)
	}
	for i := 1; i < len(ordered); i++ {
		for j := i; j > 0 && less(ordered[j], ordered[j-1]); j-- {
			ordered[j], ordered[j-1] = ordered[j-1], ordered[j]
		}
	}
	return ordered
}

// fetchSource returns (plainBody, syncedLRC). syncedLRC is non-empty only when
// the source natively carries synced lyrics.
func (c *Client) fetchSource(ctx context.Context, source Source) (string, string, error) {
	switch strings.ToLower(strings.TrimSpace(source.Type)) {
	case "booklet", "manual", "canonical", "community":
		if strings.TrimSpace(source.SyncedLRC) != "" {
			return StripTimestamps(source.SyncedLRC), source.SyncedLRC, nil
		}
		if strings.TrimSpace(source.Plain) != "" {
			return strings.TrimSpace(source.Plain), "", nil
		}
		return "", "", ErrNotFound
	case "lrclib":
		return c.fetchLrcLib(ctx, source.TrackID)
	case "zingmusic":
		return c.fetchZing(ctx, source.TrackID)
	case "jkaraoke":
		return c.fetchJKaraoke(ctx, source.FeedURL, source.SongID)
	case "jyrics", "shironet", "tab4u", "zemirotdb":
		if strings.TrimSpace(source.URL) == "" {
			return "", "", ErrNotFound
		}
		html, err := c.fetchHTML(ctx, source.URL)
		if err != nil {
			return "", "", err
		}
		return scrapeLyricsHTML(source.Type, html), "", nil
	default:
		// Unknown future source types with inline content stay usable.
		if strings.TrimSpace(source.SyncedLRC) != "" {
			return StripTimestamps(source.SyncedLRC), source.SyncedLRC, nil
		}
		if strings.TrimSpace(source.Plain) != "" {
			return strings.TrimSpace(source.Plain), "", nil
		}
		return "", "", ErrNotFound
	}
}

func (c *Client) fetchLrcLib(ctx context.Context, trackID int64) (string, string, error) {
	if trackID <= 0 {
		return "", "", ErrNotFound
	}
	endpoint := strings.TrimRight(c.lrclibBaseURL, "/") + "/get/" + itoa(trackID)
	body, err := c.fetchText(ctx, endpoint, "application/json")
	if err != nil {
		return "", "", err
	}
	var payload struct {
		SyncedLyrics string `json:"syncedLyrics"`
		PlainLyrics  string `json:"plainLyrics"`
		Instrumental bool   `json:"instrumental"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return "", "", err
	}
	if payload.Instrumental {
		return "", "", ErrNotFound
	}
	if strings.TrimSpace(payload.SyncedLyrics) != "" {
		return StripTimestamps(payload.SyncedLyrics), strings.TrimSpace(payload.SyncedLyrics), nil
	}
	if strings.TrimSpace(payload.PlainLyrics) != "" {
		return strings.TrimSpace(payload.PlainLyrics), "", nil
	}
	return "", "", ErrNotFound
}

func (c *Client) fetchZing(ctx context.Context, trackID int64) (string, string, error) {
	if trackID <= 0 {
		return "", "", ErrNotFound
	}
	query := fmt.Sprintf("{ track(where:{id:%d}){ heLyrics enLyrics } }", trackID)
	encoded, _ := json.Marshal(map[string]string{"query": query})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.zingGraphQL, bytes.NewReader(encoded))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if err := c.pace.Wait(ctx); err != nil {
		return "", "", err
	}
	response, err := c.http.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("request zingmusic: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", "", fmt.Errorf("zingmusic returned HTTP %d", response.StatusCode)
	}
	var payload struct {
		Data struct {
			Track struct {
				HeLyrics string `json:"heLyrics"`
				EnLyrics string `json:"enLyrics"`
			} `json:"track"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(&payload); err != nil {
		return "", "", err
	}
	if strings.TrimSpace(payload.Data.Track.HeLyrics) != "" {
		return strings.TrimSpace(payload.Data.Track.HeLyrics), "", nil
	}
	if strings.TrimSpace(payload.Data.Track.EnLyrics) != "" {
		return strings.TrimSpace(payload.Data.Track.EnLyrics), "", nil
	}
	return "", "", ErrNotFound
}

// fetchJKaraoke reads the karaoke feed and builds LRC from the matching song's
// lyrics array. The feed shape is provider-defined, so songs are located by a
// structural walk: any object carrying a matching song id plus a lyrics array.
func (c *Client) fetchJKaraoke(ctx context.Context, feedURL string, songID int64) (string, string, error) {
	if strings.TrimSpace(feedURL) == "" || songID <= 0 {
		return "", "", ErrNotFound
	}
	body, err := c.fetchText(ctx, feedURL, "application/json")
	if err != nil {
		return "", "", err
	}
	var decoded any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		return "", "", err
	}
	lines := findKaraokeLyrics(decoded, songID)
	if len(lines) == 0 {
		return "", "", ErrNotFound
	}
	lrc := strings.Join(lines, "\n")
	if isLRC(lrc) {
		return StripTimestamps(lrc), lrc, nil
	}
	return lrc, "", nil
}

func findKaraokeLyrics(node any, songID int64) []string {
	switch value := node.(type) {
	case map[string]any:
		if matchesSongID(value, songID) {
			if lines, ok := lyricsArray(value); ok {
				return lines
			}
		}
		for _, child := range value {
			if lines := findKaraokeLyrics(child, songID); len(lines) > 0 {
				return lines
			}
		}
	case []any:
		for _, child := range value {
			if lines := findKaraokeLyrics(child, songID); len(lines) > 0 {
				return lines
			}
		}
	}
	return nil
}

func matchesSongID(obj map[string]any, songID int64) bool {
	for _, key := range []string{"songId", "song_id", "id"} {
		if raw, ok := obj[key]; ok && toInt64(raw) == songID {
			return true
		}
	}
	return false
}

func lyricsArray(obj map[string]any) ([]string, bool) {
	for _, key := range []string{"lyrics", "lrc", "lines"} {
		raw, ok := obj[key]
		if !ok {
			continue
		}
		switch value := raw.(type) {
		case string:
			if hasBody(value) {
				return strings.Split(strings.TrimSpace(value), "\n"), true
			}
		case []any:
			lines := make([]string, 0, len(value))
			for _, item := range value {
				switch entry := item.(type) {
				case string:
					lines = append(lines, entry)
				case map[string]any:
					if text, ok := entry["text"].(string); ok {
						if time, ok := entry["time"].(string); ok && strings.TrimSpace(time) != "" {
							lines = append(lines, "["+strings.TrimSpace(time)+"]"+text)
						} else {
							lines = append(lines, text)
						}
					}
				}
			}
			if hasBody(strings.Join(lines, "\n")) {
				return lines, true
			}
		}
	}
	return nil, false
}

func toInt64(value any) int64 {
	switch number := value.(type) {
	case float64:
		return int64(number)
	case int64:
		return number
	case int:
		return int64(number)
	case json.Number:
		parsed, _ := number.Int64()
		return parsed
	}
	return 0
}

func (c *Client) fetchText(ctx context.Context, endpoint, accept string) (string, error) {
	body, err := c.fetchRaw(ctx, endpoint, accept, "")
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func (c *Client) fetchHTML(ctx context.Context, endpoint string) (string, error) {
	body, err := c.fetchRaw(ctx, endpoint, "text/html,application/json", "")
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func (c *Client) fetchRaw(ctx context.Context, endpoint, accept, _ string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", c.userAgent)
	if err := c.pace.Wait(ctx); err != nil {
		return nil, err
	}
	response, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request Zemer source: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("Zemer source returned HTTP %d", response.StatusCode)
	}
	return io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
}

var (
	tagPattern     = regexp.MustCompile(`(?s)<script.*?</script>|<style.*?</style>|<[^>]+>`)
	entityPattern  = regexp.MustCompile(`&[a-zA-Z#0-9]+;`)
	lrcLinePattern = regexp.MustCompile(`\[\d{1,3}:\d\d(?:\.\d{1,3})?\]`)
)

// scrapeLyricsHTML extracts readable lyric lines from a provider HTML page.
// Selectors differ per source, so extraction is heuristic: strip scripts,
// styles and tags, decode entities, and keep the densest block of long lines.
func scrapeLyricsHTML(sourceType, html string) string {
	text := tagPattern.ReplaceAllString(html, "\n")
	text = strings.ReplaceAll(text, "&nbsp;", " ")
	text = entityPattern.ReplaceAllString(text, " ")
	lines := make([]string, 0, 64)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}
	// Prefer the longest contiguous run of lyric-like lines (5+ words or timed).
	bestStart, bestLen := 0, 0
	start := -1
	for i, line := range lines {
		if len(strings.Fields(line)) >= 4 || lrcLinePattern.MatchString(line) {
			if start < 0 {
				start = i
			}
		} else if start >= 0 {
			if i-start > bestLen {
				bestStart, bestLen = start, i-start
			}
			start = -1
		}
	}
	if start >= 0 && len(lines)-start > bestLen {
		bestStart, bestLen = start, len(lines)-start
	}
	if bestLen >= 4 {
		return strings.Join(lines[bestStart:bestStart+bestLen], "\n")
	}
	if hasBody(strings.Join(lines, "\n")) {
		return strings.Join(lines, "\n")
	}
	return ""
}

// hasBody accepts a candidate only when it carries at least 4 non-blank lines.
func hasBody(body string) bool {
	count := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count >= 4
}

func isLRC(body string) bool {
	matched := 0
	for _, line := range strings.Split(body, "\n") {
		if lrcLinePattern.MatchString(line) {
			matched++
		}
	}
	return matched >= 2
}

// StripTimestamps returns the plain text of LRC content.
func StripTimestamps(lrc string) string {
	var b strings.Builder
	for _, line := range strings.Split(lrc, "\n") {
		text := strings.TrimSpace(lrcLinePattern.ReplaceAllString(line, ""))
		if text == "" {
			continue
		}
		b.WriteString(text)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

func itoa(value int64) string {
	return fmt.Sprintf("%d", value)
}
