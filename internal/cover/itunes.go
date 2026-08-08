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

type itunesResult struct {
	ArtworkURL100 string `json:"artworkUrl100"`
}

type itunesSearchResponse struct {
	Results []itunesResult `json:"results"`
}

// ITunes queries the iTunes Search API for album results and takes the artwork
// of the top album as artist art. iTunes has no artist artwork, so artist art
// is the highest-ranked album cover.
type ITunes struct {
	base   string
	client *jsonClient
}

// NewITunes builds an iTunes cover provider. pace spaces requests to one
// every 2 seconds (iTunes soft-caps at roughly 20 requests/min); when nil, a
// fresh 2-second pacer is used. Pass a shared pacer when several providers
// consume the same upstream host so their combined traffic stays within
// budget.
func NewITunes(baseURL, userAgent string, timeout time.Duration, pace *pacer.Pacer) (*ITunes, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = "https://itunes.apple.com"
	}
	if strings.TrimSpace(userAgent) == "" {
		return nil, fmt.Errorf("iTunes user agent is empty")
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("iTunes timeout must be positive")
	}
	if pace == nil {
		pace = pacer.New(2 * time.Second)
	}
	return &ITunes{
		base: strings.TrimRight(baseURL, "/"),
		client: &jsonClient{
			client: &http.Client{Timeout: timeout},
			agent:  userAgent,
			rate:   pace,
		},
	}, nil
}

func (c *ITunes) Name() string { return "itunes" }

func (c *ITunes) Lookup(ctx context.Context, kind Kind, input Input) (*Result, error) {
	artist := CleanArtist(input.ArtistName)
	if artist == "" {
		return nil, ErrNotFound
	}
	params := url.Values{}
	params.Set("entity", "album")
	params.Set("limit", "5")
	switch kind {
	case Artist:
		params.Set("term", artist)
	case Album:
		album := CleanAlbum(input.AlbumName)
		if album == "" {
			return nil, ErrNotFound
		}
		params.Set("term", artist+" "+album)
	default:
		return nil, ErrNotFound
	}
	endpoint, err := encodeQuery(c.base, "/search", params)
	if err != nil {
		return nil, err
	}
	var response itunesSearchResponse
	if err := c.client.get(ctx, endpoint, &response); err != nil {
		return nil, err
	}
	for _, result := range response.Results {
		if value := upgradeITunes(result.ArtworkURL100); value != "" {
			return &Result{URL: value, Source: c.Name()}, nil
		}
	}
	return nil, ErrNotFound
}

// upgradeITunes rewrites a 100x100 artwork URL to 600x600. Empty inputs are
// preserved. iTunes returns 100x100 thumbnails but accepts reusing the same URL
// with a larger size segment.
func upgradeITunes(value string) string {
	if value == "" {
		return ""
	}
	return strings.ReplaceAll(strings.ReplaceAll(value, "100x100bb", "600x600bb"), "100x100", "600x600")
}
