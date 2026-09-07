package lyricsplus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sillygru/music-utils/internal/names"
	"github.com/sillygru/music-utils/internal/pacer"
	"github.com/sillygru/music-utils/internal/ttml"
)

// ErrNotFound reports that LyricsPlus has no lyrics for the track.
var ErrNotFound = errors.New("lyricsplus lyrics not found")

const maxResponseBytes = 8 << 20

var isrcPattern = regexp.MustCompile(`^[A-Z]{2}[A-Z0-9]{3}\d{2}\d{5}$`)

// Result is one LyricsPlus lookup outcome.
type Result struct {
	TrackName    string
	ArtistName   string
	AlbumName    string
	PlainLyrics  string
	SyncedLyrics string
	WordSynced   bool
	TTML         string
	RichJSON     string
}

// Client fetches TTML via the Binimum index and structured lyrics via
// LyricsPlus mirrors.
type Client struct {
	apiBaseURL string
	mirrors    []string
	userAgent  string
	http       *http.Client
	pace       *pacer.Pacer

	mu          sync.Mutex
	working hint
}

type hint struct {
	url string
}

// New creates a client. apiBaseURL is the Binimum index
// (https://lyrics-api.binimum.org); mirrors are LyricsPlus /v2/lyrics/get hosts.
func New(apiBaseURL string, mirrors []string, userAgent string, timeout time.Duration) (*Client, error) {
	apiBaseURL = strings.TrimRight(strings.TrimSpace(apiBaseURL), "/")
	parsed, err := url.Parse(apiBaseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("invalid LyricsPlus API URL")
	}
	cleaned := make([]string, 0, len(mirrors))
	for _, mirror := range mirrors {
		mirror = strings.TrimRight(strings.TrimSpace(mirror), "/")
		if parsed, err := url.Parse(mirror); err == nil && parsed.Host != "" && (parsed.Scheme == "http" || parsed.Scheme == "https") {
			cleaned = append(cleaned, mirror)
		}
	}
	if strings.TrimSpace(userAgent) == "" || timeout <= 0 {
		return nil, fmt.Errorf("LyricsPlus user agent and timeout are required")
	}
	return &Client{
		apiBaseURL: apiBaseURL,
		mirrors:    cleaned,
		userAgent:  userAgent,
		http:       &http.Client{Timeout: timeout},
		pace:       pacer.New(300 * time.Millisecond),
	}, nil
}

// DefaultMirrors is the Metrolist mirror list (workers host disabled: daily cap).
func DefaultMirrors() []string {
	return []string{
		"https://lyricsplus.binimum.org",
		"https://lyricsplus.atomix.one/",
		"https://lyricsplus.prjktla.my.id",
		"https://lyricsplus-seven.vercel.app",
	}
}

// Get resolves lyrics: word-synced Binimum TTML wins outright, otherwise the
// mirror result is used (preferring word-synced over line-synced).
func (c *Client) Get(ctx context.Context, trackName, artistName, albumName string, duration float64, isrc string) (*Result, error) {
	if c == nil || c.http == nil {
		return nil, errors.New("LyricsPlus client is nil")
	}
	input := names.Normalize(trackName, artistName, albumName)
	if input.TrackName == "" {
		return nil, ErrNotFound
	}
	binimum, binimumErr := c.getBinimum(ctx, input, duration, isrc)
	if binimum != nil && binimum.WordSynced {
		return binimum, nil
	}
	mirror, mirrorErr := c.getMirror(ctx, input, duration)
	return resolveWithFallback(binimum, mirror, binimumErr, mirrorErr)
}

func resolveWithFallback(binimum, mirror *Result, binimumErr, mirrorErr error) (*Result, error) {
	if mirror != nil && mirror.WordSynced {
		return mirror, nil
	}
	if mirror != nil {
		return mirror, nil
	}
	if binimum != nil {
		return binimum, nil
	}
	if mirrorErr != nil && !errors.Is(mirrorErr, ErrNotFound) {
		return nil, mirrorErr
	}
	if binimumErr != nil {
		return nil, binimumErr
	}
	return nil, ErrNotFound
}

type binimumResult struct {
	LyricsURL  string `json:"lyricsUrl"`
	TimingType string `json:"timing_type"`
}

func (c *Client) getBinimum(ctx context.Context, input names.Input, duration float64, isrc string) (*Result, error) {
	endpoint, err := url.Parse(c.apiBaseURL + "/")
	if err != nil {
		return nil, err
	}
	query := endpoint.Query()
	isrc = strings.ToUpper(strings.TrimSpace(isrc))
	if isrcPattern.MatchString(isrc) {
		query.Set("isrc", isrc)
	} else {
		query.Set("track", input.TrackName)
		query.Set("artist", input.ArtistName)
		if input.AlbumName != "" {
			query.Set("album", input.AlbumName)
		}
		if duration > 0 {
			query.Set("duration", strconv.FormatInt(int64(duration*1000), 10))
		}
	}
	endpoint.RawQuery = query.Encode()
	// When an ISRC is available the metadata query is the documented fallback.
	queries := []string{endpoint.String()}
	if isrcPattern.MatchString(isrc) {
		fallback, _ := url.Parse(c.apiBaseURL + "/")
		fallbackQuery := fallback.Query()
		fallbackQuery.Set("track", input.TrackName)
		fallbackQuery.Set("artist", input.ArtistName)
		if input.AlbumName != "" {
			fallbackQuery.Set("album", input.AlbumName)
		}
		if duration > 0 {
			fallbackQuery.Set("duration", strconv.FormatInt(int64(duration*1000), 10))
		}
		fallback.RawQuery = fallbackQuery.Encode()
		queries = append(queries, fallback.String())
	}
	var lastErr error = ErrNotFound
	for _, target := range queries {
		var payload struct {
			Results []binimumResult `json:"results"`
		}
		if err := c.doJSON(ctx, target, &payload); err != nil {
			lastErr = err
			continue
		}
		for _, item := range payload.Results {
			if strings.TrimSpace(item.LyricsURL) == "" {
				continue
			}
			ttmlDoc, err := c.fetchText(ctx, item.LyricsURL)
			if err != nil {
				lastErr = err
				continue
			}
			synced, err := ttml.ParseToLRC(ttmlDoc)
			if err != nil || strings.TrimSpace(synced) == "" {
				continue
			}
			wordSynced := strings.EqualFold(strings.TrimSpace(item.TimingType), "word")
			if !wordSynced {
				if lines, parseErr := ttml.Parse(ttmlDoc); parseErr == nil {
					for _, line := range lines {
						if len(line.Words) > 0 {
							wordSynced = true
							break
						}
					}
				}
			}
			return &Result{
				TrackName: input.TrackName, ArtistName: input.ArtistName, AlbumName: input.AlbumName,
				PlainLyrics: ttml.PlainText(ttmlDoc), SyncedLyrics: synced,
				WordSynced: wordSynced, TTML: ttmlDoc,
			}, nil
		}
	}
	return nil, lastErr
}

type mirrorLine struct {
	Time     float64 `json:"time"`
	Duration float64 `json:"duration"`
	Text     string  `json:"text"`
	Syllabi  []struct {
		Time         float64 `json:"time"`
		Duration     float64 `json:"duration"`
		Text         string  `json:"text"`
		IsBackground bool    `json:"isBackground"`
	} `json:"syllabus"`
	Element *struct {
		Singer string `json:"singer"`
	} `json:"element"`
}

func (c *Client) getMirror(ctx context.Context, input names.Input, duration float64) (*Result, error) {
	ordered := c.orderedMirrors()
	var lastErr error = ErrNotFound
	for _, mirror := range ordered {
		result, err := c.getOneMirror(ctx, mirror, input, duration)
		if err != nil {
			lastErr = err
			continue
		}
		c.mu.Lock()
		c.working.url = mirror
		c.mu.Unlock()
		return result, nil
	}
	return nil, lastErr
}

func (c *Client) orderedMirrors() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.working.url == "" {
		return append([]string(nil), c.mirrors...)
	}
	ordered := make([]string, 0, len(c.mirrors))
	ordered = append(ordered, c.working.url)
	for _, mirror := range c.mirrors {
		if mirror != c.working.url {
			ordered = append(ordered, mirror)
		}
	}
	return ordered
}

func (c *Client) getOneMirror(ctx context.Context, mirror string, input names.Input, duration float64) (*Result, error) {
	endpoint, err := url.Parse(mirror + "/v2/lyrics/get")
	if err != nil {
		return nil, err
	}
	query := endpoint.Query()
	query.Set("title", input.TrackName)
	query.Set("artist", input.ArtistName)
	if duration > 0 {
		query.Set("duration", strconv.FormatInt(int64(duration), 10))
	}
	if input.AlbumName != "" {
		query.Set("album", input.AlbumName)
	}
	endpoint.RawQuery = query.Encode()
	var payload struct {
		Type   string       `json:"type"`
		Lyrics []mirrorLine `json:"lyrics"`
	}
	if err := c.doJSON(ctx, endpoint.String(), &payload); err != nil {
		return nil, err
	}
	if len(payload.Lyrics) == 0 {
		return nil, ErrNotFound
	}
	wordMode := strings.EqualFold(strings.TrimSpace(payload.Type), "word")
	synced, plain := convertMirrorLines(payload.Lyrics, wordMode)
	if strings.TrimSpace(synced) == "" && strings.TrimSpace(plain) == "" {
		return nil, ErrNotFound
	}
	wordSynced := wordMode && strings.TrimSpace(synced) != ""
	richJSON := ""
	if wordSynced {
		richJSON = mirrorLinesToRichJSON(payload.Lyrics)
		// If conversion fails, keep wordSynced false so fallback to LRC only.
		if richJSON == "" {
			wordSynced = false
		}
	}
	return &Result{
		TrackName: input.TrackName, ArtistName: input.ArtistName, AlbumName: input.AlbumName,
		PlainLyrics: plain, SyncedLyrics: synced, WordSynced: wordSynced, RichJSON: richJSON,
	}, nil
}

// convertMirrorLines builds extended LRC: {agent:vN} multi-voice tags, {bg}
// for the first line of each background run. Word-level blocks are dropped in
// favor of line timing to keep the LRC playable everywhere.
func convertMirrorLines(lines []mirrorLine, wordMode bool) (synced, plain string) {
	agents := map[string]bool{}
	for _, line := range lines {
		if line.Element != nil && strings.TrimSpace(line.Element.Singer) != "" {
			agents[strings.TrimSpace(line.Element.Singer)] = true
		}
	}
	multiAgent := len(agents) > 1
	if len(agents) == 1 {
		for alias := range agents {
			if alias != "v1" {
				multiAgent = true
			}
		}
	}
	var syncedLines, plainLines []string
	inBackgroundRun := false
	for _, line := range lines {
		text := strings.TrimSpace(line.Text)
		if text == "" {
			continue
		}
		background := false
		for _, syllable := range line.Syllabi {
			if syllable.IsBackground {
				background = true
				break
			}
		}
		plainLines = append(plainLines, text)
		tag := ""
		if multiAgent && line.Element != nil && strings.TrimSpace(line.Element.Singer) != "" {
			tag += "{agent:" + strings.TrimSpace(line.Element.Singer) + "}"
		}
		if background {
			if !inBackgroundRun {
				tag += "{bg}"
			}
			inBackgroundRun = true
		} else {
			inBackgroundRun = false
		}
		minutes := int(line.Time) / 60
		seconds := line.Time - float64(minutes*60)
		syncedLines = append(syncedLines, formatLRCLine(minutes, seconds, tag+text))
	}
	_ = wordMode
	return strings.Join(syncedLines, "\n"), strings.Join(plainLines, "\n")
}

func formatLRCLine(minutes int, seconds float64, text string) string {
	return "[" + formatTwo(minutes) + ":" + formatSec(seconds) + "]" + text
}

func formatTwo(value int) string {
	if value < 10 {
		return "0" + strconv.Itoa(value)
	}
	return strconv.Itoa(value)
}

func formatSec(value float64) string {
	whole := int(value)
	centis := int((value - float64(whole)) * 100)
	if centis < 0 {
		centis = 0
	}
	sec := formatTwo(whole)
	cent := formatTwo(centis)
	return sec + "." + cent
}

// mirrorLinesToRichJSON converts word-mode mirror lines into compact rich JSON.
// Format matches httpserver.compactRichSync {lines:[[begin,end,text,[[begin,end,text]]]]}.
func mirrorLinesToRichJSON(lines []mirrorLine) string {
	type wordTuple [3]any
	type lineTuple [4]any
	compactLines := make([]lineTuple, 0, len(lines))
	var maxEnd float64
	for _, line := range lines {
		text := strings.TrimSpace(line.Text)
		if text == "" {
			continue
		}
		words := make([]wordTuple, 0, len(line.Syllabi))
		for _, s := range line.Syllabi {
			wText := strings.TrimSpace(s.Text)
			if wText == "" {
				continue
			}
			wBegin := s.Time
			wEnd := s.Time + s.Duration
			if s.Duration <= 0 {
				wEnd = wBegin + 0.5
			}
			words = append(words, wordTuple{wBegin, wEnd, wText})
			if wEnd > maxEnd {
				maxEnd = wEnd
			}
		}
		begin := line.Time
		end := line.Time + line.Duration
		if line.Duration <= 0 {
			if len(words) > 0 {
				end = words[len(words)-1][1].(float64)
			} else {
				end = begin + 2
			}
		}
		if end > maxEnd {
			maxEnd = end
		}
		// Ensure words is non-nil for JSON.
		if words == nil {
			words = []wordTuple{}
		}
		compactLines = append(compactLines, lineTuple{begin, end, text, words})
	}
	if len(compactLines) == 0 {
		return ""
	}
	payload := map[string]any{
		"lines": compactLines,
	}
	// Include duration for completeness.
	if maxEnd > 0 {
		payload["duration"] = maxEnd
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func (c *Client) doJSON(ctx context.Context, endpoint string, target any) error {
	body, err := c.fetchText(ctx, endpoint)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(body), target); err != nil {
		return fmt.Errorf("decode LyricsPlus response: %w", err)
	}
	return nil
}

func (c *Client) fetchText(ctx context.Context, endpoint string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if err := c.pace.Wait(ctx); err != nil {
		return "", err
	}
	response, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("request LyricsPlus: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return "", ErrNotFound
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("LyricsPlus returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return "", err
	}
	return string(body), nil
}
