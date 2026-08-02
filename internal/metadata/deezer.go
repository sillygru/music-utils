package metadata

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

	"github.com/sillygru/music-utils/internal/db"
)

type deezerAlbum struct {
	Title   string `json:"title"`
	CoverBig string `json:"cover_big"`
}

type deezerTrack struct {
	ID          int64        `json:"id"`
	Title       string       `json:"title"`
	Artist      struct{ Name string } `json:"artist"`
	Album       deezerAlbum  `json:"album"`
	Duration    float64      `json:"duration"`
	ISRC        string       `json:"isrc"`
	ReleaseDate string       `json:"release_date"`
}

type deezerSearchResponse struct {
	Data []deezerTrack `json:"data"`
}

// Deezer queries the public Deezer API, which exposes metadata, ISRC, and a
// cover URL without authentication.
type Deezer struct {
	baseURL   string
	userAgent string
	client    *http.Client
}

func NewDeezer(baseURL, userAgent string, timeout time.Duration) (*Deezer, error) {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://api.deezer.com"
	}
	if strings.TrimSpace(userAgent) == "" {
		return nil, errors.New("Deezer user agent is empty")
	}
	if timeout <= 0 {
		return nil, errors.New("Deezer timeout must be positive")
	}
	return &Deezer{
		baseURL:   strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		userAgent: userAgent,
		client:    &http.Client{Timeout: timeout},
	}, nil
}

func (c *Deezer) Name() string { return "deezer" }

func (c *Deezer) Lookup(ctx context.Context, input Input) (*db.Track, error) {
	endpoint, err := url.Parse(c.baseURL + "/search")
	if err != nil {
		return nil, fmt.Errorf("build Deezer URL: %w", err)
	}
	query := endpoint.Query()
	query.Set("q", deezerQuery(input))
	endpoint.RawQuery = query.Encode()

	var response deezerSearchResponse
	if err := c.do(ctx, endpoint.String(), &response); err != nil {
		return nil, err
	}
	candidate := bestDeezer(response.Data, input)
	if candidate == nil {
		return nil, ErrNotFound
	}
	return trackFromDeezer(candidate, input), nil
}

func deezerQuery(input Input) string {
	var parts []string
	if track := strings.TrimSpace(input.TrackName); track != "" {
		parts = append(parts, `track:"`+strings.ReplaceAll(track, `"`, `\"`) + `"`)
	}
	if artist := strings.TrimSpace(input.ArtistName); artist != "" {
		parts = append(parts, `artist:"`+strings.ReplaceAll(artist, `"`, `\"`) + `"`)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ")
}

func (c *Deezer) do(ctx context.Context, endpoint string, value any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("create Deezer request: %w", err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")
	response, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("request Deezer: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Deezer returned HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(value); err != nil {
		return fmt.Errorf("decode Deezer response: %w", err)
	}
	return nil
}

func bestDeezer(tracks []deezerTrack, input Input) *deezerTrack {
	if len(tracks) == 0 {
		return nil
	}
	best := &tracks[0]
	bestScore := deezerScore(*best, input)
	for i := 1; i < len(tracks); i++ {
		score := deezerScore(tracks[i], input)
		if score > bestScore {
			best = &tracks[i]
			bestScore = score
		}
	}
	if bestScore < 0 {
		return nil
	}
	return best
}

func deezerScore(candidate deezerTrack, input Input) int {
	score := 0
	title, artist := normalize(candidate.Title), normalize(candidate.Artist.Name)
	inputTrack, inputArtist := normalize(input.TrackName), normalize(input.ArtistName)
	if title == inputTrack {
		score += 100
	} else if !strings.Contains(title, inputTrack) && !strings.Contains(inputTrack, title) {
		score -= 100
	}
	if artist == inputArtist {
		score += 100
	} else if !strings.Contains(artist, inputArtist) && !strings.Contains(inputArtist, artist) {
		score -= 100
	}
	if input.Duration > 0 && candidate.Duration > 0 && abs(candidate.Duration-input.Duration) <= 2 {
		score += 20
	}
	return score
}

func trackFromDeezer(candidate *deezerTrack, input Input) *db.Track {
	track := &db.Track{
		Name:            firstNonEmpty(candidate.Title, input.TrackName),
		ArtistName:      firstNonEmpty(candidate.Artist.Name, input.ArtistName),
		Duration:        input.Duration,
		ISRC:            candidate.ISRC,
		MetadataSource:  "deezer",
		Source:          "deezer",
		MetadataChecked: true,
		CoverURLChecked: true,
	}
	if candidate.Duration > 0 {
		track.Duration = candidate.Duration
	}
	track.AlbumName = candidate.Album.Title
	track.ReleaseDate = releaseDateOnly(candidate.ReleaseDate)
	track.Year = releaseYear(candidate.ReleaseDate)
	if strings.TrimSpace(candidate.Album.CoverBig) != "" {
		track.CoverURL = candidate.Album.CoverBig
		track.CoverURLSource = "deezer"
	}
	return track
}