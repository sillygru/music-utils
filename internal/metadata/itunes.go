package metadata

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

	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/pacer"
)

const maxResponseBytes = 4 << 20

type iTunesResult struct {
	TrackName        string `json:"trackName"`
	ArtistName       string `json:"artistName"`
	CollectionName   string `json:"collectionName"`
	TrackTimeMillis  int64  `json:"trackTimeMillis"`
	ReleaseDate      string `json:"releaseDate"`
	PrimaryGenreName string `json:"primaryGenreName"`
	ArtworkURL100    string `json:"artworkUrl100"`
	ArtworkURL600    string `json:"artworkUrl600"`
	ISRC             string `json:"isrc"`
}

type iTunesSearchResponse struct {
	ResultCount int            `json:"resultCount"`
	Results     []iTunesResult `json:"results"`
}

// ITunes queries the iTunes Search API, which returns metadata and a cover
// artwork URL in a single unauthenticated request. Requests are paced to one
// every 2 seconds (iTunes soft-caps at roughly 20 calls/min).
type ITunes struct {
	baseURL   string
	userAgent string
	client    *http.Client
	pace      *pacer.Pacer
}

// NewITunes builds an iTunes metadata provider. pace spaces requests to one
// every 2 seconds (iTunes soft-caps at roughly 20 calls/min); when nil, a
// fresh 2-second pacer is used. Pass a shared pacer when several providers
// consume the same upstream host so their combined traffic stays within
// budget.
func NewITunes(baseURL, userAgent string, timeout time.Duration, pace *pacer.Pacer) (*ITunes, error) {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://itunes.apple.com"
	}
	if strings.TrimSpace(userAgent) == "" {
		return nil, errors.New("iTunes user agent is empty")
	}
	if timeout <= 0 {
		return nil, errors.New("iTunes timeout must be positive")
	}
	if pace == nil {
		pace = pacer.New(2 * time.Second)
	}
	return &ITunes{
		baseURL:   strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		userAgent: userAgent,
		client:    &http.Client{Timeout: timeout},
		pace:      pace,
	}, nil
}

func (c *ITunes) Name() string { return "itunes" }

func (c *ITunes) Lookup(ctx context.Context, input Input) (*db.Track, error) {
	endpoint, err := url.Parse(c.baseURL + "/search")
	if err != nil {
		return nil, fmt.Errorf("build iTunes URL: %w", err)
	}
	query := endpoint.Query()
	query.Set("media", "music")
	query.Set("entity", "song")
	query.Set("limit", "10")
	query.Set("country", "US")
	// Combine the fields so the API surfaces the right song reliably. iTunes
	// does not accept separate structured artist/track terms.
	term := strings.TrimSpace(input.TrackName)
	if artist := strings.TrimSpace(input.ArtistName); artist != "" {
		term += " " + artist
	}
	query.Set("term", term)
	endpoint.RawQuery = query.Encode()

	var response iTunesSearchResponse
	if err := c.do(ctx, endpoint.String(), &response); err != nil {
		return nil, err
	}
	candidate := bestITunes(response.Results, input)
	if candidate == nil {
		return nil, ErrNotFound
	}
	return trackFromITunes(candidate, input), nil
}

func (c *ITunes) do(ctx context.Context, endpoint string, value any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("create iTunes request: %w", err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")
	if err := c.pace.Wait(ctx); err != nil {
		return err
	}
	response, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("request iTunes: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("iTunes returned HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(value); err != nil {
		return fmt.Errorf("decode iTunes response: %w", err)
	}
	return nil
}

func bestITunes(results []iTunesResult, input Input) *iTunesResult {
	if len(results) == 0 {
		return nil
	}
	best := &results[0]
	bestScore := itunesScore(*best, input)
	for i := 1; i < len(results); i++ {
		score := itunesScore(results[i], input)
		if score > bestScore {
			best = &results[i]
			bestScore = score
		}
	}
	if bestScore < 0 {
		return nil
	}
	return best
}

func itunesScore(candidate iTunesResult, input Input) int {
	score := 0
	trackName, artistName := normalize(candidate.TrackName), normalize(candidate.ArtistName)
	inputTrack, inputArtist := normalize(input.TrackName), normalize(input.ArtistName)
	if trackName == inputTrack {
		score += 100
	} else if !strings.Contains(trackName, inputTrack) && !strings.Contains(inputTrack, trackName) {
		score -= 100
	}
	if artistName == inputArtist {
		score += 100
	} else if !strings.Contains(artistName, inputArtist) && !strings.Contains(inputArtist, artistName) {
		score -= 100
	}
	if input.Duration > 0 && candidate.TrackTimeMillis > 0 && abs(float64(candidate.TrackTimeMillis)/1000-input.Duration) <= 2 {
		score += 20
	}
	return score
}

func trackFromITunes(candidate *iTunesResult, input Input) *db.Track {
	track := &db.Track{
		Name:            firstNonEmpty(candidate.TrackName, input.TrackName),
		ArtistName:      firstNonEmpty(candidate.ArtistName, input.ArtistName),
		Duration:        input.Duration,
		ISRC:            candidate.ISRC,
		MetadataSource:  "itunes",
		Source:          "itunes",
		MetadataChecked: true,
		CoverURLChecked: true,
	}
	if candidate.TrackTimeMillis > 0 {
		track.Duration = float64(candidate.TrackTimeMillis) / 1000
	}
	if candidate.CollectionName != "" {
		track.AlbumName = candidate.CollectionName
	}
	track.Genre = candidate.PrimaryGenreName
	track.ReleaseDate = releaseDateOnly(candidate.ReleaseDate)
	track.Year = releaseYear(candidate.ReleaseDate)
	track.CoverURL, track.CoverURLSource = coverFromITunes(candidate)
	return track
}

// coverFromITunes returns a larger artwork URL and its source when iTunes
// offered one for free; otherwise both are empty so the field stays empty.
// iTunes returns 100x100 thumbnails but accepts rewriting the size segment of
// the same URL to a larger resolution, so a 300x300 variant is preferred.
func coverFromITunes(candidate *iTunesResult) (string, string) {
	base := firstNonEmpty(candidate.ArtworkURL600, candidate.ArtworkURL100)
	if base == "" {
		return "", ""
	}
	if candidate.ArtworkURL100 == base || (candidate.ArtworkURL600 == "" && candidate.ArtworkURL100 != "") {
		base = strings.ReplaceAll(base, "100x100", "300x300")
	}
	return base, "itunes"
}

func releaseDateOnly(value string) string {
	if len(value) < 10 {
		return value
	}
	return value[:10]
}

func releaseYear(value string) int {
	if len(value) < 4 {
		return 0
	}
	year, _ := strconv.Atoi(value[:4])
	return year
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
