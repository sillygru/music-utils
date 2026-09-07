package innertube

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sillygru/music-utils/internal/pacer"
)

// ErrNotFound reports that YouTube has no lyrics for the video.
var ErrNotFound = errors.New("youtube lyrics not found")

const maxResponseBytes = 8 << 20

// webClientKey is the public YouTube web client API key embedded in YouTube's
// own web app. It is not a secret; deployments may override it via config.
const webClientKey = "AIzaSyC9XL3ZjWddXya6X74dJoCTL-WEYFDNX30"

// LyricsResult is one YouTube lyrics lookup outcome.
type LyricsResult struct {
	PlainLyrics  string
	SyncedLyrics string
}

// Client talks to InnerTube (music.youtube.com/youtubei/v1) for official
// lyrics shelves and caption transcripts.
type Client struct {
	baseURL   string
	apiKey    string
	userAgent string
	http      *http.Client
	pace      *pacer.Pacer
}

// New creates a client. baseURL is the InnerTube root
// (https://music.youtube.com/youtubei/v1); apiKey may be empty to use the
// public web client key.
func New(baseURL, apiKey, userAgent string, timeout time.Duration) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("invalid InnerTube base URL")
	}
	if strings.TrimSpace(apiKey) == "" {
		apiKey = webClientKey
	}
	if strings.TrimSpace(userAgent) == "" || timeout <= 0 {
		return nil, fmt.Errorf("InnerTube user agent and timeout are required")
	}
	return &Client{
		baseURL:   baseURL,
		apiKey:    strings.TrimSpace(apiKey),
		userAgent: userAgent,
		http:      &http.Client{Timeout: timeout},
		pace:      pacer.New(500 * time.Millisecond),
	}, nil
}

// GetOfficialLyrics resolves the lyrics shelf for a video: next() yields the
// lyrics browse endpoint, browse() returns the musicDescriptionShelfRenderer
// text. This is YouTube Music's official lyric shelf (usually unsynced).
func (c *Client) GetOfficialLyrics(ctx context.Context, videoID string) (*LyricsResult, error) {
	if c == nil || c.http == nil {
		return nil, errors.New("InnerTube client is nil")
	}
	videoID = strings.TrimSpace(videoID)
	if videoID == "" {
		return nil, ErrNotFound
	}
	nextBody, err := c.post(ctx, "/next", map[string]any{
		"context": webRemixContext(),
		"videoId": videoID,
	})
	if err != nil {
		return nil, err
	}
	browseID, params := findLyricsEndpoint(nextBody)
	if browseID == "" {
		return nil, ErrNotFound
	}
	browseBody, err := c.post(ctx, "/browse", map[string]any{
		"context":  webRemixContext(),
		"browseId": browseID,
		"params":   params,
	})
	if err != nil {
		return nil, err
	}
	text := findShelfText(browseBody)
	if strings.TrimSpace(text) == "" {
		return nil, ErrNotFound
	}
	return &LyricsResult{PlainLyrics: strings.TrimSpace(text)}, nil
}

// GetTranscript downloads the video's captions via the player endpoint and
// converts the first caption track to LRC.
func (c *Client) GetTranscript(ctx context.Context, videoID string) (*LyricsResult, error) {
	if c == nil || c.http == nil {
		return nil, errors.New("InnerTube client is nil")
	}
	videoID = strings.TrimSpace(videoID)
	if videoID == "" {
		return nil, ErrNotFound
	}
	playerBody, err := c.post(ctx, "/player", map[string]any{
		"context": webPlayerContext(),
		"videoId": videoID,
	})
	if err != nil {
		return nil, err
	}
	trackURL := findCaptionTrack(playerBody)
	if trackURL == "" {
		return nil, ErrNotFound
	}
	timedText, err := c.get(ctx, trackURL)
	if err != nil {
		return nil, err
	}
	synced := transcriptToLRC(timedText)
	if strings.TrimSpace(synced) == "" {
		return nil, ErrNotFound
	}
	return &LyricsResult{PlainLyrics: stripTimestamps(synced), SyncedLyrics: synced}, nil
}

func webRemixContext() map[string]any {
	return map[string]any{"client": map[string]any{
		"clientName": "WEB_REMIX", "clientVersion": "1.20240101.00.00",
		"hl": "en", "gl": "US",
	}}
}

func webPlayerContext() map[string]any {
	return map[string]any{"client": map[string]any{
		"clientName": "WEB", "clientVersion": "2.20240101.00.00",
		"hl": "en", "gl": "US",
	}}
}

func (c *Client) post(ctx context.Context, path string, payload map[string]any) (map[string]any, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	endpoint := c.baseURL + path + "?key=" + url.QueryEscape(c.apiKey) + "&prettyPrint=false"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Origin", "https://music.youtube.com")
	req.Header.Set("Referer", "https://music.youtube.com/")
	if err := c.pace.Wait(ctx); err != nil {
		return nil, err
	}
	response, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request InnerTube: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("InnerTube returned HTTP %d", response.StatusCode)
	}
	var decoded map[string]any
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode InnerTube response: %w", err)
	}
	return decoded, nil
}

func (c *Client) get(ctx context.Context, endpoint string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/xml,text/xml")
	req.Header.Set("User-Agent", c.userAgent)
	if err := c.pace.Wait(ctx); err != nil {
		return "", err
	}
	response, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("request captions: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("captions returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// findLyricsEndpoint walks a next() response for the lyrics browse endpoint.
// Lyrics shelves use browseIds starting with "MPLYt".
func findLyricsEndpoint(node map[string]any) (browseID, params string) {
	var bestID, bestParams string
	walkJSON(node, func(obj map[string]any) bool {
		endpoint, ok := obj["browseEndpoint"].(map[string]any)
		if !ok {
			return false
		}
		id, _ := endpoint["browseId"].(string)
		if !strings.HasPrefix(id, "MPLYt") {
			return false
		}
		bestID = id
		bestParams, _ = endpoint["params"].(string)
		return true
	})
	return bestID, bestParams
}

// findShelfText walks a browse() response for the lyrics shelf text.
func findShelfText(node map[string]any) string {
	var text string
	walkJSON(node, func(obj map[string]any) bool {
		shelf, ok := obj["musicDescriptionShelfRenderer"].(map[string]any)
		if !ok {
			return false
		}
		if description, ok := shelf["description"].(map[string]any); ok {
			if runs, ok := description["runs"].([]any); ok {
				var b strings.Builder
				for _, run := range runs {
					if item, ok := run.(map[string]any); ok {
						if fragment, ok := item["text"].(string); ok {
							b.WriteString(fragment)
						}
					}
				}
				if strings.TrimSpace(b.String()) != "" {
					text = b.String()
					return true
				}
			}
		}
		return false
	})
	return text
}

// findCaptionTrack walks a player() response for the first caption track URL.
func findCaptionTrack(node map[string]any) string {
	var trackURL string
	walkJSON(node, func(obj map[string]any) bool {
		captions, ok := obj["captions"].(map[string]any)
		if !ok {
			return false
		}
		list, ok := captions["playerCaptionsTracklistRenderer"].(map[string]any)
		if !ok {
			return false
		}
		tracks, ok := list["captionTracks"].([]any)
		if !ok || len(tracks) == 0 {
			return false
		}
		if first, ok := tracks[0].(map[string]any); ok {
			if baseURL, ok := first["baseUrl"].(string); ok && strings.TrimSpace(baseURL) != "" {
				trackURL = baseURL
				return true
			}
		}
		return false
	})
	return trackURL
}

func walkJSON(node any, visit func(map[string]any) bool) bool {
	switch value := node.(type) {
	case map[string]any:
		if visit(value) {
			return true
		}
		for _, child := range value {
			if walkJSON(child, visit) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if walkJSON(child, visit) {
				return true
			}
		}
	}
	return false
}

type timedText struct {
	Paragraphs []timedPara `xml:"p"`
	Texts      []timedLine `xml:"text"`
}

type timedPara struct {
	Start string `xml:"t,attr"`
	Dur   string `xml:"d,attr"`
	Text  string `xml:",chardata"`
	P     []timedPara `xml:"p"`
	S     []timedSpan `xml:"s"`
}

type timedSpan struct {
	Text string `xml:",chardata"`
}

type timedLine struct {
	Start string `xml:"start,attr"`
	Dur   string `xml:"dur,attr"`
	Text  string `xml:",chardata"`
}

// transcriptToLRC converts YouTube timedtext XML (both <transcript><text> and
// TTML-ish <timedtext><p> shapes) to LRC.
func transcriptToLRC(body string) string {
	var lines []lrcLine
	var doc timedText
	if err := xml.Unmarshal([]byte(body), &doc); err == nil {
		for _, item := range doc.Texts {
			start, err1 := strconv.ParseFloat(strings.TrimSpace(item.Start), 64)
			if err1 != nil {
				continue
			}
			text := strings.TrimSpace(htmlUnescape(item.Text))
			text = strings.Join(strings.Fields(text), " ")
			if text == "" {
				continue
			}
			lines = append(lines, lrcLine{start: start, text: text})
		}
		for _, para := range doc.Paragraphs {
			startMS, err1 := strconv.ParseFloat(strings.TrimSpace(para.Start), 64)
			if err1 != nil {
				continue
			}
			text := strings.TrimSpace(htmlUnescape(para.Text))
			if text == "" {
				continue
			}
			lines = append(lines, lrcLine{start: startMS / 1000, text: strings.Join(strings.Fields(text), " ")})
		}
	}
	var b strings.Builder
	for _, line := range lines {
		minutes := int(line.start) / 60
		seconds := line.start - float64(minutes*60)
		fmt.Fprintf(&b, "[%02d:%05.2f]%s\n", minutes, seconds, line.text)
	}
	return b.String()
}

type lrcLine struct {
	start float64
	text  string
}

func htmlUnescape(value string) string {
	replacer := strings.NewReplacer(
		"&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'",
		"&nbsp;", " ",
	)
	return replacer.Replace(value)
}

func stripTimestamps(lrc string) string {
	var b strings.Builder
	for _, line := range strings.Split(lrc, "\n") {
		text := strings.TrimSpace(line)
		if idx := strings.Index(text, "]"); idx >= 0 && strings.HasPrefix(text, "[") {
			text = strings.TrimSpace(text[idx+1:])
		}
		if text == "" {
			continue
		}
		b.WriteString(text)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}
