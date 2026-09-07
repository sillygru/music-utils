package paxsenix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sillygru/music-utils/internal/names"
	"github.com/sillygru/music-utils/internal/pacer"
	"github.com/sillygru/music-utils/internal/ttml"
)

// ErrNotFound reports that Paxsenix has no lyrics for the track.
var ErrNotFound = errors.New("paxsenix lyrics not found")

const maxResponseBytes = 8 << 20

var (
	indexJS    = regexp.MustCompile(`/assets/index~[^/"]+\.js`)
	jwtPattern = regexp.MustCompile(`eyJ[A-Za-z0-9\-_=]+\.[A-Za-z0-9\-_=]+\.[A-Za-z0-9\-_=]+`)
)

// Result is one Paxsenix lookup outcome.
type Result struct {
	TrackName    string
	ArtistName   string
	AlbumName    string
	Duration     float64
	PlainLyrics  string
	SyncedLyrics string
	WordSynced   bool
	TTML         string
}

// Client resolves Apple Music track ids and fetches lyrics via the Paxsenix proxy.
type Client struct {
	proxyBaseURL   string
	appleBaseURL   string
	catalogBaseURL string
	userAgent      string
	http           *http.Client
	pace           *pacer.Pacer
	tokens         *tokenManager
}

// New creates a client. proxyBaseURL is the Paxsenix host
// (https://lyrics.paxsenix.org); appleBaseURL is Apple's site used only for
// Bearer-token scraping (https://beta.music.apple.com).
func New(proxyBaseURL, appleBaseURL, userAgent string, timeout time.Duration) (*Client, error) {
	proxyBaseURL = strings.TrimRight(strings.TrimSpace(proxyBaseURL), "/")
	appleBaseURL = strings.TrimRight(strings.TrimSpace(appleBaseURL), "/")
	for _, raw := range []string{proxyBaseURL, appleBaseURL} {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return nil, fmt.Errorf("invalid Paxsenix URL")
		}
	}
	if strings.TrimSpace(userAgent) == "" || timeout <= 0 {
		return nil, fmt.Errorf("Paxsenix user agent and timeout are required")
	}
	httpClient := &http.Client{Timeout: timeout}
	return &Client{
		proxyBaseURL:   proxyBaseURL,
		appleBaseURL:   appleBaseURL,
		catalogBaseURL: "https://amp-api.music.apple.com",
		userAgent:      userAgent,
		http:           httpClient,
		pace:           pacer.New(500 * time.Millisecond),
		tokens:         newTokenManager(appleBaseURL, httpClient),
	}, nil
}

// NewWithToken creates a client with a fixed Bearer token (tests, or
// deployments that manage the Apple token out of band).
func NewWithToken(proxyBaseURL, userAgent, token string, timeout time.Duration) (*Client, error) {
	client, err := New(proxyBaseURL, "https://beta.music.apple.com", userAgent, timeout)
	if err != nil {
		return nil, err
	}
	client.tokens.fixed = strings.TrimSpace(token)
	return client, nil
}

// Get searches Apple Music for the track, then fetches lyrics from Paxsenix.
func (c *Client) Get(ctx context.Context, trackName, artistName, albumName string) (*Result, error) {
	if c == nil || c.http == nil {
		return nil, errors.New("Paxsenix client is nil")
	}
	input := names.Normalize(trackName, artistName, albumName)
	if input.TrackName == "" {
		return nil, ErrNotFound
	}
	token, err := c.tokens.get(ctx)
	if err != nil {
		return nil, err
	}
	songs, err := c.searchApple(ctx, token, input)
	if err != nil {
		return nil, err
	}
	best := chooseSong(songs, input)
	if best == nil {
		return nil, ErrNotFound
	}
	return c.lyricsForID(ctx, best)
}

type appleSong struct {
	ID         string
	Name       string
	ArtistName string
	AlbumName  string
	Duration   float64
}

func (c *Client) searchApple(ctx context.Context, token string, input names.Input) ([]appleSong, error) {
	endpoint, err := url.Parse(strings.TrimRight(c.catalogBaseURL, "/") + "/v1/catalog/us/search")
	if err != nil {
		return nil, err
	}
	query := endpoint.Query()
	query.Set("term", strings.Join(nonEmpty(input.TrackName, input.ArtistName), " "))
	query.Set("types", "songs")
	query.Set("limit", "25")
	query.Set("l", "en-US")
	query.Set("platform", "web")
	query.Set("format[resources]", "map")
	query.Set("include[songs]", "artists")
	query.Set("extend", "artistUrl")
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Origin", "https://music.apple.com")
	req.Header.Set("Referer", "https://music.apple.com/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:95.0) Gecko/20100101 Firefox/95.0")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")
	req.Header.Set("x-apple-renewal", "true")
	if err := c.pace.Wait(ctx); err != nil {
		return nil, err
	}
	response, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request Apple catalog: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		c.tokens.clear()
		return nil, ErrNotFound
	}
	if response.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("Apple catalog returned HTTP %d", response.StatusCode)
	}
	var payload struct {
		Results struct {
			Songs struct {
				Data []struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"data"`
			} `json:"songs"`
		} `json:"results"`
		Resources struct {
			Songs map[string]struct {
				Attributes struct {
					Name            string  `json:"name"`
					ArtistName      string  `json:"artistName"`
					AlbumName       string  `json:"albumName"`
					DurationInMills float64 `json:"durationInMillis"`
				} `json:"attributes"`
			} `json:"songs"`
		} `json:"resources"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode Apple catalog response: %w", err)
	}
	out := make([]appleSong, 0, len(payload.Results.Songs.Data))
	for _, item := range payload.Results.Songs.Data {
		detail, ok := payload.Resources.Songs[item.ID]
		if !ok {
			continue
		}
		out = append(out, appleSong{
			ID:         item.ID,
			Name:       detail.Attributes.Name,
			ArtistName: detail.Attributes.ArtistName,
			AlbumName:  detail.Attributes.AlbumName,
			Duration:   detail.Attributes.DurationInMills / 1000,
		})
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out, nil
}

func chooseSong(songs []appleSong, input names.Input) *appleSong {
	var best *appleSong
	bestScore := -1.0
	for i := range songs {
		candidate := names.Normalize(songs[i].Name, songs[i].ArtistName, songs[i].AlbumName)
		score := similarityScore(candidate.TrackName, input.TrackName)*3 +
			similarityScore(candidate.ArtistName, input.ArtistName)*3
		if input.AlbumName != "" {
			score += similarityScore(candidate.AlbumName, input.AlbumName)
		}
		if score > bestScore {
			bestScore = score
			best = &songs[i]
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
		return 0.75
	}
	return 0
}

type lyricsPayload struct {
	Type        string          `json:"type"`
	TTMLContent string          `json:"ttmlContent"`
	ELRCMulti   string          `json:"elrcMultiPerson"`
	ELRC        string          `json:"elrc"`
	Plain       string          `json:"plain"`
	Content     []lyricsContent `json:"content"`
}

type lyricsContent struct {
	Timestamp    int64       `json:"timestamp"`
	Text         []lyricText `json:"text"`
	Background   bool        `json:"background"`
	OppositeTurn bool        `json:"oppositeTurn"`
}

type lyricText struct {
	Text string `json:"text"`
}

func (c *Client) lyricsForID(ctx context.Context, song *appleSong) (*Result, error) {
	endpoint, err := url.Parse(c.proxyBaseURL + "/apple-music/lyrics")
	if err != nil {
		return nil, err
	}
	query := endpoint.Query()
	query.Set("id", song.ID)
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")
	if err := c.pace.Wait(ctx); err != nil {
		return nil, err
	}
	response, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request Paxsenix: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("Paxsenix returned HTTP %d", response.StatusCode)
	}
	var payload lyricsPayload
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode Paxsenix response: %w", err)
	}
	result := &Result{TrackName: song.Name, ArtistName: song.ArtistName, AlbumName: song.AlbumName, Duration: song.Duration}
	// Priority: ttmlContent > elrcMultiPerson > elrc > plain > content[].
	if strings.TrimSpace(payload.TTMLContent) != "" {
		if lines, err := ttml.Parse(payload.TTMLContent); err == nil {
			synced := ttml.ToLRC(lines)
			if strings.TrimSpace(synced) != "" {
				result.SyncedLyrics = ttml.CleanSyncedLyrics(synced)
				result.PlainLyrics = ttml.PlainText(payload.TTMLContent)
				if result.PlainLyrics == "" {
					result.PlainLyrics = ttml.ExtractPlainFromLRC(result.SyncedLyrics)
				}
				hasWords := false
				for _, l := range lines {
					if len(l.Words) > 0 {
						hasWords = true
						break
					}
				}
				result.WordSynced = hasWords
				result.TTML = payload.TTMLContent
				return result, nil
			}
		}
	}
	if strings.TrimSpace(payload.ELRCMulti) != "" {
		result.SyncedLyrics = ttml.CleanSyncedLyrics(strings.TrimSpace(payload.ELRCMulti))
		result.PlainLyrics = ttml.ExtractPlainFromLRC(result.SyncedLyrics)
		result.WordSynced = true
		return result, nil
	}
	if strings.TrimSpace(payload.ELRC) != "" {
		result.SyncedLyrics = ttml.CleanSyncedLyrics(strings.TrimSpace(payload.ELRC))
		result.PlainLyrics = ttml.ExtractPlainFromLRC(result.SyncedLyrics)
		return result, nil
	}
	if strings.TrimSpace(payload.Plain) != "" {
		result.PlainLyrics = strings.TrimSpace(payload.Plain)
		return result, nil
	}
	if len(payload.Content) > 0 {
		synced, plain, wordSynced := buildFromContent(payload)
		if strings.TrimSpace(synced) != "" || strings.TrimSpace(plain) != "" {
			result.SyncedLyrics = ttml.CleanSyncedLyrics(synced)
			result.PlainLyrics = plain
			if result.PlainLyrics == "" {
				result.PlainLyrics = ttml.ExtractPlainFromLRC(result.SyncedLyrics)
			}
			result.WordSynced = wordSynced
			return result, nil
		}
	}
	return nil, ErrNotFound
}

func buildFromContent(payload lyricsPayload) (synced, plain string, wordSynced bool) {
	wordSynced = strings.EqualFold(strings.TrimSpace(payload.Type), "syllable")
	var syncedLines, plainLines []string
	for _, item := range payload.Content {
		words := make([]string, 0, len(item.Text))
		for _, word := range item.Text {
			if strings.TrimSpace(word.Text) != "" {
				words = append(words, word.Text)
			}
		}
		text := strings.TrimSpace(strings.Join(words, " "))
		if text == "" {
			continue
		}
		plainLines = append(plainLines, text)
		if wordSynced {
			syncedLines = append(syncedLines, formatMS(item.Timestamp)+text)
		} else {
			syncedLines = append(syncedLines, text)
		}
	}
	if wordSynced {
		return strings.Join(syncedLines, "\n"), strings.Join(plainLines, "\n"), true
	}
	return "", strings.Join(plainLines, "\n"), false
}

func formatMS(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	minutes := ms / 60000
	seconds := (ms % 60000) / 1000
	centis := (ms % 1000) / 10
	return fmt.Sprintf("[%02d:%02d.%02d]", minutes, seconds, centis)
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

// tokenManager scrapes and caches the Apple Music web Bearer token.
type tokenManager struct {
	baseURL string
	http    *http.Client
	mu      sync.Mutex
	token   string
	fixed   string
}

func newTokenManager(baseURL string, httpClient *http.Client) *tokenManager {
	return &tokenManager{baseURL: baseURL, http: httpClient}
}

func (m *tokenManager) get(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fixed != "" {
		return m.fixed, nil
	}
	if m.token != "" {
		return m.token, nil
	}
	token, err := m.scrape(ctx)
	if err != nil {
		return "", err
	}
	m.token = token
	return token, nil
}

func (m *tokenManager) clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.token = ""
}

func (m *tokenManager) scrape(ctx context.Context) (string, error) {
	home, err := m.fetchBody(ctx, m.baseURL)
	if err != nil {
		return "", err
	}
	jsURI := indexJS.FindString(home)
	if jsURI == "" {
		return "", fmt.Errorf("paxsenix: Apple index bundle not found")
	}
	bundle, err := m.fetchBody(ctx, strings.TrimRight(m.baseURL, "/")+jsURI)
	if err != nil {
		return "", err
	}
	token := jwtPattern.FindString(bundle)
	if token == "" {
		return "", fmt.Errorf("paxsenix: Apple token not found")
	}
	return token, nil
}

func (m *tokenManager) fetchBody(ctx context.Context, endpoint string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept", "text/html,application/javascript")
	response, err := m.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("request Apple site: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("Apple site returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return "", err
	}
	return string(body), nil
}
