package betterlyrics

import (
	"bufio"
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

var ErrNotFound = errors.New("better lyrics not found")

type Result struct {
	Source   string
	Content  string
	Format   string
	SyncType string
}

type Client struct {
	baseURL   string
	userAgent string
	token     string
	http      *http.Client
	pace      *pacer.Pacer
}

func New(baseURL, userAgent, token string, timeout time.Duration) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("invalid Better Lyrics base URL")
	}
	if strings.TrimSpace(userAgent) == "" || timeout <= 0 {
		return nil, fmt.Errorf("Better Lyrics user agent and timeout are required")
	}
	return &Client{
		baseURL: baseURL, userAgent: userAgent, token: strings.TrimSpace(token),
		http: &http.Client{Timeout: timeout}, pace: pacer.New(200 * time.Millisecond),
	}, nil
}

// Stream opens one unified request and emits every provider result as soon as
// it arrives. The caller should keep the stream alive after its HTTP waiter
// returns so late provider variants can be cached.
func (c *Client) Stream(ctx context.Context, song, artist, album string, duration float64, emit func(Result)) error {
	if c == nil || c.http == nil {
		return errors.New("Better Lyrics client is nil")
	}
	input := names.Normalize(song, artist, album)
	endpoint, err := url.Parse(c.baseURL + "/v2/lyrics")
	if err != nil {
		return err
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
		return err
	}
	req.Header.Set("Accept", "text/event-stream, application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if err := c.pace.Wait(ctx); err != nil {
		return err
	}
	response, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Better Lyrics returned HTTP %d", response.StatusCode)
	}
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	if strings.Contains(contentType, "event-stream") {
		return c.readSSE(response.Body, emit)
	}
	return c.readJSON(response.Body, emit)
}

func (c *Client) readSSE(body io.Reader, emit func(Result)) error {
	scanner := bufio.NewScanner(io.LimitReader(body, 16<<20))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if data.Len() > 0 {
				c.emitJSON(data.String(), emit)
				data.Reset()
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if data.Len() > 0 {
		c.emitJSON(data.String(), emit)
	}
	return scanner.Err()
}

func (c *Client) readJSON(body io.Reader, emit func(Result)) error {
	var value any
	if err := json.NewDecoder(io.LimitReader(body, 16<<20)).Decode(&value); err != nil {
		return err
	}
	return emitValue(value, emit)
}

func (c *Client) emitJSON(raw string, emit func(Result)) {
	if strings.TrimSpace(raw) == "[DONE]" {
		return
	}
	var value any
	if json.Unmarshal([]byte(raw), &value) == nil {
		_ = emitValue(value, emit)
	}
}

func emitValue(value any, emit func(Result)) error {
	if object, ok := value.(map[string]any); ok {
		if data, ok := object["data"]; ok {
			return emitValue(data, emit)
		}
		provider, _ := object["provider"].(string)
		if results, ok := object["results"].(map[string]any); ok {
			return emitProvider(provider, results, emit)
		}
		if lyrics, ok := object["lyrics"].(string); ok && lyrics != "" {
			return emitProvider(provider, map[string]any{"lyrics": lyrics, "format": object["format"], "syncType": object["syncType"]}, emit)
		}
	}
	if list, ok := value.([]any); ok {
		for _, item := range list {
			if err := emitValue(item, emit); err != nil {
				return err
			}
		}
	}
	return nil
}

func emitProvider(provider string, results map[string]any, emit func(Result)) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		provider = "better_lyrics"
	}
	if raw, ok := results["wordByWord"].(string); ok && raw != "" {
		emit(Result{Source: "musixmatch", Content: raw, Format: "lrc", SyncType: "word"})
	}
	if raw, ok := results["synced"].(string); ok && raw != "" {
		emit(Result{Source: "musixmatch", Content: raw, Format: "lrc", SyncType: "line"})
	}
	raw, _ := results["lyrics"].(string)
	if raw == "" {
		return nil
	}
	if decoded, ok := decodeNestedLyrics(raw); ok {
		raw = decoded
	}
	source, format, syncType := provider, "ttml", "line"
	switch provider {
	case "qq", "portato":
		source, format, syncType = "better_lyrics_portato", "qrc", "word"
	case "kugou", "legato":
		source, format, syncType = "better_lyrics_legato", "lrc", "line"
	case "golyrics", "better_lyrics":
		source, format, syncType = "better_lyrics", "ttml", "syllable"
	case "binimum", "binilyrics":
		source, format, syncType = "binilyrics", "ttml", "syllable"
	}
	if value, ok := results["timingType"].(string); ok && strings.EqualFold(value, "line") {
		syncType = "line"
	}
	emit(Result{Source: source, Content: raw, Format: format, SyncType: syncType})
	return nil
}

func decodeNestedLyrics(value string) (string, bool) {
	var object map[string]any
	if json.Unmarshal([]byte(value), &object) != nil {
		return "", false
	}
	for _, key := range []string{"ttml", "lyrics", "lrc", "content"} {
		if text, ok := object[key].(string); ok && text != "" {
			return text, true
		}
	}
	return "", false
}
