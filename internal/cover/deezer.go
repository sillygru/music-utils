package cover

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type deezerArtist struct {
	PictureXL     string `json:"picture_xl"`
	PictureBig    string `json:"picture_big"`
	PictureMedium string `json:"picture_medium"`
}

type deezerAlbum struct {
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

// NewDeezer builds a Deezer cover provider.
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
		client: &jsonClient{client: &http.Client{Timeout: timeout}, agent: userAgent},
	}, nil
}

func (c *Deezer) Name() string { return "deezer" }

func (c *Deezer) Lookup(ctx context.Context, kind Kind, input Input) (*Result, error) {
	artist := CleanArtist(input.ArtistName)
	if artist == "" {
		return nil, ErrNotFound
	}
	switch kind {
	case Artist:
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
				return &Result{URL: value, Source: c.Name()}, nil
			}
		}
		return nil, ErrNotFound
	case Album:
		album := CleanAlbum(input.AlbumName)
		if album == "" {
			return nil, ErrNotFound
		}
		params := url.Values{}
		params.Set("q", `"`+artist+" "+album+`"`)
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
				return &Result{URL: value, Source: c.Name()}, nil
			}
		}
		return nil, ErrNotFound
	default:
		return nil, ErrNotFound
	}
}
