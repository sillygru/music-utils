package musicbrainz

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
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/sillygru/music-utils/internal/db"
)

var ErrNotFound = errors.New("musicbrainz recording not found")

const maxResponseBytes = 4 << 20

type Client struct {
	musicBrainzURL string
	coverArtURL    string
	userAgent      string
	http           *http.Client

	requestLimiter *requestLimiter
	inflightMu     sync.Mutex
	inflight       map[string]*lookupCall
}

type lookupCall struct {
	done  chan struct{}
	track *db.Track
	err   error
}

type Input struct {
	Name     string
	Artist   string
	Album    string
	Duration float64
}

type recordingSearchResponse struct {
	Recordings []recording `json:"recordings"`
}

type recording struct {
	ID           string         `json:"id"`
	Title        string         `json:"title"`
	Length       int            `json:"length"`
	Score        int            `json:"score"`
	ArtistCredit []artistCredit `json:"artist-credit"`
	Releases     []release      `json:"releases"`
	Genres       []tag          `json:"genres"`
	Tags         []tag          `json:"tags"`
	ISRCs        []string       `json:"isrcs"`
}

type artistCredit struct {
	Name   string `json:"name"`
	Artist struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"artist"`
}

type release struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Date         string `json:"date"`
	ReleaseGroup struct {
		ID string `json:"id"`
	} `json:"release-group"`
}

type tag struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type coverResponse struct {
	Images []coverImage `json:"images"`
}

type coverImage struct {
	Front      bool              `json:"front"`
	Image      string            `json:"image"`
	Thumbnails map[string]string `json:"thumbnails"`
}

// New creates a client with the application's default request limits.
// NewWithRateLimits should be used by the server so provider traffic follows
// its configured API limits.
func New(musicBrainzURL, coverArtURL, userAgent string, timeout time.Duration) (*Client, error) {
	return NewWithRateLimits(musicBrainzURL, coverArtURL, userAgent, timeout, 10, 180)
}

// NewWithRateLimits creates a client whose outbound provider calls use the
// same per-second burst and rolling-minute limits as the HTTP API.
func NewWithRateLimits(musicBrainzURL, coverArtURL, userAgent string, timeout time.Duration, perSecond, perMinute int) (*Client, error) {
	musicBrainzURL = strings.TrimRight(strings.TrimSpace(musicBrainzURL), "/")
	coverArtURL = strings.TrimRight(strings.TrimSpace(coverArtURL), "/")
	for name, value := range map[string]string{"MusicBrainz": musicBrainzURL, "Cover Art Archive": coverArtURL} {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return nil, fmt.Errorf("invalid %s base URL", name)
		}
	}
	if strings.TrimSpace(userAgent) == "" {
		return nil, errors.New("MusicBrainz user agent is empty")
	}
	if timeout <= 0 {
		return nil, errors.New("MusicBrainz timeout must be positive")
	}
	if perSecond < 1 || perMinute < 1 {
		return nil, errors.New("MusicBrainz request limits must be positive")
	}
	transport := http.DefaultTransport
	if defaultTransport, ok := http.DefaultTransport.(*http.Transport); ok {
		optimized := defaultTransport.Clone()
		optimized.MaxIdleConns = 100
		optimized.MaxIdleConnsPerHost = 20
		optimized.IdleConnTimeout = 90 * time.Second
		transport = optimized
	}
	return &Client{
		musicBrainzURL: musicBrainzURL,
		coverArtURL:    coverArtURL,
		userAgent:      userAgent,
		http:           &http.Client{Timeout: timeout, Transport: transport},
		requestLimiter: newRequestLimiter(perSecond, perMinute),
		inflight:       make(map[string]*lookupCall),
	}, nil
}

// Lookup finds the best recording match and, when a release is available,
// resolves a front-cover URL from Cover Art Archive. Identical concurrent
// lookups share one upstream operation.
func (c *Client) Lookup(ctx context.Context, input Input) (*db.Track, error) {
	if c == nil || c.http == nil {
		return nil, errors.New("MusicBrainz client is nil")
	}
	key := normalize(input.Name) + "\x00" + normalize(input.Artist) + "\x00" + normalize(input.Album)
	c.inflightMu.Lock()
	if call, ok := c.inflight[key]; ok {
		c.inflightMu.Unlock()
		select {
		case <-call.done:
			return call.track, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &lookupCall{done: make(chan struct{})}
	c.inflight[key] = call
	c.inflightMu.Unlock()

	call.track, call.err = c.lookup(ctx, input)
	close(call.done)
	c.inflightMu.Lock()
	delete(c.inflight, key)
	c.inflightMu.Unlock()
	return call.track, call.err
}

func (c *Client) lookup(ctx context.Context, input Input) (*db.Track, error) {
	query := `recording:"` + escapeQuery(input.Name) + `" AND artist:"` + escapeQuery(input.Artist) + `"`
	endpoint, err := url.Parse(c.musicBrainzURL + "/recording/")
	if err != nil {
		return nil, fmt.Errorf("build MusicBrainz URL: %w", err)
	}
	values := endpoint.Query()
	values.Set("query", query)
	values.Set("fmt", "json")
	values.Set("limit", "10")
	values.Set("inc", "releases+artist-credits+genres+tags")
	endpoint.RawQuery = values.Encode()

	var response recordingSearchResponse
	if err := c.doMusicBrainz(ctx, endpoint.String(), &response); err != nil {
		return nil, err
	}
	candidate := chooseRecording(response.Recordings, input)
	if candidate == nil {
		return nil, ErrNotFound
	}
	track := &db.Track{
		Name:                   firstNonEmpty(candidate.Title, input.Name),
		ArtistName:             firstNonEmpty(artistName(candidate), input.Artist),
		AlbumName:              firstNonEmpty(firstReleaseTitle(candidate), input.Album),
		Duration:               input.Duration,
		MusicBrainzRecordingID: candidate.ID,
		MetadataSource:         "musicbrainz",
		Source:                 "musicbrainz",
		MetadataChecked:        true,
		CoverURLChecked:        true,
	}
	if candidate.Length > 0 {
		track.Duration = float64(candidate.Length) / 1000
	}
	if first := firstRelease(candidate); first != nil {
		track.MusicBrainzReleaseID = first.ID
		track.ReleaseDate = first.Date
		track.Year = releaseYear(first.Date)
		track.MusicBrainzReleaseGroupID = first.ReleaseGroup.ID
		track.CoverURL = c.frontCover(ctx, first.ID)
		if track.CoverURL != "" {
			track.CoverURLSource = "cover_art_archive"
		}
	}
	track.MusicBrainzArtistID = artistID(candidate)
	track.ISRC = firstNonEmpty(candidate.ISRCs...)
	track.Genre = firstTag(candidate.Genres, candidate.Tags)
	return track, nil
}

type requestLimiter struct {
	mu        sync.Mutex
	perSecond *rate.Limiter
	perMinute int
	requests  []time.Time
}

func newRequestLimiter(perSecond, perMinute int) *requestLimiter {
	return &requestLimiter{
		perSecond: rate.NewLimiter(rate.Limit(perSecond), perSecond),
		perMinute: perMinute,
		requests:  make([]time.Time, 0, perMinute),
	}
}

// Wait reserves one outbound provider request under both configured limits.
// The per-second limiter permits the same burst as the HTTP API, while the
// rolling-minute window prevents a burst from exceeding the minute ceiling.
func (l *requestLimiter) Wait(ctx context.Context) error {
	if l == nil {
		return errors.New("MusicBrainz request limiter is nil")
	}
	for {
		now := time.Now()
		l.mu.Lock()
		cutoff := now.Add(-time.Minute)
		firstRecent := 0
		for firstRecent < len(l.requests) && !l.requests[firstRecent].After(cutoff) {
			firstRecent++
		}
		l.requests = l.requests[firstRecent:]
		if len(l.requests) >= l.perMinute {
			wait := time.Until(l.requests[0].Add(time.Minute))
			l.mu.Unlock()
			if err := waitContext(ctx, wait); err != nil {
				return err
			}
			continue
		}
		l.mu.Unlock()

		if err := l.perSecond.Wait(ctx); err != nil {
			return err
		}

		l.mu.Lock()
		now = time.Now()
		cutoff = now.Add(-time.Minute)
		firstRecent = 0
		for firstRecent < len(l.requests) && !l.requests[firstRecent].After(cutoff) {
			firstRecent++
		}
		l.requests = l.requests[firstRecent:]
		if len(l.requests) < l.perMinute {
			l.requests = append(l.requests, now)
			l.mu.Unlock()
			return nil
		}
		l.mu.Unlock()
	}
}

func waitContext(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) doMusicBrainz(ctx context.Context, endpoint string, value any) error {
	if err := c.requestLimiter.Wait(ctx); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("create MusicBrainz request: %w", err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("request MusicBrainz: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("MusicBrainz returned HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(value); err != nil {
		return fmt.Errorf("decode MusicBrainz response: %w", err)
	}
	return nil
}

func (c *Client) frontCover(ctx context.Context, releaseID string) string {
	if releaseID == "" {
		return ""
	}
	endpoint := c.coverArtURL + "/release/" + url.PathEscape(releaseID)
	if err := c.requestLimiter.Wait(ctx); err != nil {
		return ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		return ""
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return ""
	}
	var payload coverResponse
	if json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(&payload) != nil {
		return ""
	}
	for _, image := range payload.Images {
		if !image.Front {
			continue
		}
		for _, size := range []string{"500", "250", "1200"} {
			if value := strings.TrimSpace(image.Thumbnails[size]); value != "" {
				return value
			}
		}
		return image.Image
	}
	return ""
}

func chooseRecording(recordings []recording, input Input) *recording {
	if len(recordings) == 0 {
		return nil
	}
	best := &recordings[0]
	bestScore := matchScore(*best, input)
	for i := 1; i < len(recordings); i++ {
		score := matchScore(recordings[i], input)
		if score > bestScore {
			best = &recordings[i]
			bestScore = score
		}
	}
	if bestScore < 0 {
		return nil
	}
	return best
}

func matchScore(candidate recording, input Input) int {
	score := candidate.Score
	candidateTitle, inputTitle := normalize(candidate.Title), normalize(input.Name)
	if candidateTitle == inputTitle {
		score += 100
	} else if !strings.Contains(candidateTitle, inputTitle) && !strings.Contains(inputTitle, candidateTitle) {
		score -= 100
	}
	candidateArtist, inputArtist := normalize(artistName(&candidate)), normalize(input.Artist)
	if candidateArtist == inputArtist {
		score += 100
	} else if !strings.Contains(candidateArtist, inputArtist) && !strings.Contains(inputArtist, candidateArtist) {
		score -= 100
	}
	if input.Duration > 0 && candidate.Length > 0 && abs(float64(candidate.Length)/1000-input.Duration) <= 2 {
		score += 20
	}
	return score
}

func firstRelease(candidate *recording) *release {
	if len(candidate.Releases) == 0 {
		return nil
	}
	return &candidate.Releases[0]
}

func firstReleaseTitle(candidate *recording) string {
	if release := firstRelease(candidate); release != nil {
		return release.Title
	}
	return ""
}

func artistName(candidate *recording) string {
	if len(candidate.ArtistCredit) == 0 {
		return ""
	}
	if candidate.ArtistCredit[0].Name != "" {
		return candidate.ArtistCredit[0].Name
	}
	return candidate.ArtistCredit[0].Artist.Name
}

func artistID(candidate *recording) string {
	if len(candidate.ArtistCredit) == 0 {
		return ""
	}
	return candidate.ArtistCredit[0].Artist.ID
}

func firstTag(groups ...[]tag) string {
	var best tag
	for _, group := range groups {
		for _, item := range group {
			if item.Count > best.Count || best.Name == "" {
				best = item
			}
		}
	}
	return best.Name
}

func releaseYear(value string) int {
	if len(value) < 4 {
		return 0
	}
	year, _ := strconv.Atoi(value[:4])
	return year
}

func escapeQuery(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(value), `\`, `\\`), `"`, `\"`)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func normalize(value string) string { return strings.ToLower(strings.TrimSpace(value)) }
func abs(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}
