package cover

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sillygru/music-utils/internal/pacer"
)

type deezerArtist struct {
	Name          string `json:"name"`
	PictureXL     string `json:"picture_xl"`
	PictureBig    string `json:"picture_big"`
	PictureMedium string `json:"picture_medium"`
}

type deezerAlbum struct {
	Title       string `json:"title"`
	CoverXL     string `json:"cover_xl"`
	CoverBig    string `json:"cover_big"`
	CoverMedium string `json:"cover_medium"`
}

type deezerArtistResponse struct {
	Data []deezerArtist `json:"data"`
}

type deezerAlbumResponse struct {
	Data []deezerAlbum `json:"data"`
}

// Deezer queries the public Deezer API for album and artist artwork. It honors
// no explicit rate limiter in code; Deezer permits roughly 50 requests per 5s
// and always returns high-resolution _xl art in practice.
type Deezer struct {
	base   string
	client *jsonClient
}

// NewDeezer builds a Deezer cover provider. Requests are paced to one every
// 2 seconds even though Deezer permits roughly 50 per 5s, because the provider
// is a tertiary fallback that is rarely hit and conservatism is free.
func NewDeezer(baseURL, userAgent string, timeout time.Duration) (*Deezer, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = "https://api.deezer.com"
	}
	if strings.TrimSpace(userAgent) == "" {
		return nil, fmt.Errorf("Deezer user agent is empty")
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("Deezer timeout must be positive")
	}
	return &Deezer{
		base:   strings.TrimRight(baseURL, "/"),
		client: &jsonClient{client: &http.Client{Timeout: timeout}, agent: userAgent, rate: pacer.New(2 * time.Second)},
	}, nil
}

func (c *Deezer) Name() string { return "deezer" }

func (c *Deezer) Lookup(ctx context.Context, kind Kind, input Input) (*Result, error) {
	artist := CleanArtist(input.ArtistName)
	switch kind {
	case Song:
		track := strings.TrimSpace(input.TrackName)
		if track == "" {
			return nil, ErrNotFound
		}
		params := url.Values{}
		q := `track:"` + strings.ReplaceAll(track, `"`, `"`) + `"`
		if artist != "" {
			q += ` artist:"` + strings.ReplaceAll(artist, `"`, `"`) + `"`
		}
		params.Set("q", q)
		endpoint, err := encodeQuery(c.base, "/search", params)
		if err != nil {
			return nil, err
		}
		var response struct {
			Data []struct {
				Title  string       `json:"title"`
				Artist deezerArtist `json:"artist"`
				Album  deezerAlbum  `json:"album"`
			} `json:"data"`
		}
		if err := c.client.get(ctx, endpoint, &response); err != nil {
			return nil, err
		}
		for _, result := range response.Data {
			if value := firstNonEmpty(result.Album.CoverXL, result.Album.CoverBig, result.Album.CoverMedium); value != "" {
				return &Result{URL: value, Source: c.Name(), TrackName: result.Title, ArtistName: result.Artist.Name, AlbumName: result.Album.Title}, nil
			}
		}
		return nil, ErrNotFound
	case Artist:
		if artist == "" {
			return nil, ErrNotFound
		}
		params := url.Values{}
		params.Set("q", artist)
		endpoint, err := encodeQuery(c.base, "/search/artist", params)
		if err != nil {
			return nil, err
		}
		var response deezerArtistResponse
		if err := c.client.get(ctx, endpoint, &response); err != nil {
			return nil, err
		}
		for _, result := range response.Data {
			if value := firstNonEmpty(result.PictureXL, result.PictureBig, result.PictureMedium); value != "" {
				return &Result{URL: value, Source: c.Name(), ArtistName: result.Name}, nil
			}
		}
		return nil, ErrNotFound
	case Album:
		album := CleanAlbum(input.AlbumName)
		if album == "" {
			return nil, ErrNotFound
		}
		params := url.Values{}
		params.Set("q", `"`+strings.TrimSpace(artist+" "+album)+`"`)
		endpoint, err := encodeQuery(c.base, "/search/album", params)
		if err != nil {
			return nil, err
		}
		var response deezerAlbumResponse
		if err := c.client.get(ctx, endpoint, &response); err != nil {
			return nil, err
		}
		for _, result := range response.Data {
			if value := firstNonEmpty(result.CoverXL, result.CoverBig, result.CoverMedium); value != "" {
				return &Result{URL: value, Source: c.Name(), AlbumName: result.Title}, nil
			}
		}
		return nil, ErrNotFound
	default:
		return nil, ErrNotFound
	}
}
