package cover

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Last.fm scrapes artist/album pages over HTTPS and regex-extracts image URLs.
// There is no public JSON API and no official rate limit; roughly 2 requests
// per second is a safe estimate, so the provider paces itself to 1 per 2s.
type Lastfm struct {
	base   string
	client *http.Client
	agent  string
	rate   *intervalLimiter
}

// lastfmRegexes are tried in order; the first match that isn't a placeholder
// wins because earlier patterns tend to have higher-quality artwork.
var lastfmRegexes = []*regexp.Regexp{
	// GIF avatars, highest quality.
	regexp.MustCompile(`src="([^"]+)"[^>]*alt="gif"`),
	// OpenGraph image, attribute order 1.
	regexp.MustCompile(`<meta property="og:image" content="([^"]+)"`),
	// OpenGraph image, attribute order 2.
	regexp.MustCompile(`<meta content="([^"]+)" property="og:image"`),
	// Any fastly avatar image URL.
	regexp.MustCompile(`src="(https?://[^"]*lastfm[^"]*fastly[^"]*/i/u/[^"]+)"`),
}

// lastfmDummyHashes are placeholder/dummy image hashes that must be rejected.
var lastfmDummyHashes = []string{
	"2a9cbd8b46e8849cf0f1c91b013a0edf",
	"a17431e7e8b04c9b8b2f1c8f7e2c7f48",
	"noimage",
	" placeholder",
}

// NewLastfm builds a Last.fm scraping cover provider.
func NewLastfm(baseURL, userAgent string, timeout time.Duration) (*Lastfm, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = "https://www.last.fm"
	}
	if strings.TrimSpace(userAgent) == "" {
		return nil, fmt.Errorf("Last.fm user agent is empty")
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("Last.fm timeout must be positive")
	}
	return &Lastfm{
		base:   strings.TrimRight(baseURL, "/"),
		client: &http.Client{Timeout: timeout},
		agent:  userAgent,
		rate:   &intervalLimiter{interval: 2 * time.Second},
	}, nil
}

func (c *Lastfm) Name() string { return "lastfm" }

func (c *Lastfm) Lookup(ctx context.Context, kind Kind, input Input) (*Result, error) {
	artist := CleanArtist(input.ArtistName)
	if artist == "" {
		return nil, ErrNotFound
	}
	var pages []string
	switch kind {
	case Artist:
		pages = []string{
			"/music/" + lastfmPath(artist) + "/+images",
			"/music/" + lastfmPath(artist),
		}
	case Album:
		album := CleanAlbum(input.AlbumName)
		if album == "" {
			return nil, ErrNotFound
		}
		pages = []string{
			"/music/" + lastfmPath(artist) + "/" + lastfmPath(album) + "/+images",
			"/music/" + lastfmPath(artist) + "/" + lastfmPath(album),
		}
	default:
		return nil, ErrNotFound
	}
	for _, page := range pages {
		value, err := c.searchPage(ctx, page)
		if err != nil {
			continue
		}
		value = upgradeLastfmArtworkURL(value)
		if value != "" {
			return &Result{URL: value, Source: c.Name()}, nil
		}
	}
	return nil, ErrNotFound
}

// searchPage fetches a Last.fm page and returns the first valid image URL.
func (c *Lastfm) searchPage(ctx context.Context, page string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+page, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", c.agent)
	if err := c.rate.Wait(ctx); err != nil {
		return "", err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Last.fm returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", err
	}
	return extractLastfmImageURL(string(body)), nil
}

// lastfmPath percent-encodes a path segment, using '+' as the space separator
// like Last.fm's own slugs.
func lastfmPath(value string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	for _, r := range value {
		switch {
		case r == ' ':
			b.WriteByte('+')
		case r == '-' || r == '_' || r == '.' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			b.WriteRune(r)
		default:
			b.WriteString(fmt.Sprintf("%%%02X", r))
		}
	}
	return b.String()
}

// extractLastfmImageURL runs the regexes in order and returns the first
// candidate that is not a placeholder URL.
func extractLastfmImageURL(body string) string {
	for _, re := range lastfmRegexes {
		match := re.FindStringSubmatch(body)
		if len(match) < 2 {
			continue
		}
		if !isLastfmDummy(match[1]) {
			return match[1]
		}
	}
	return ""
}

// isLastfmDummy reports whether a URL is a known placeholder/dummy image.
func isLastfmDummy(value string) bool {
	lower := strings.ToLower(value)
	for _, dummy := range lastfmDummyHashes {
		if strings.Contains(lower, dummy) {
			return true
		}
	}
	return false
}

// upgradeLastfmArtworkURL rewrites Last.fm CDN resolution segments to a shared
// 300x300 resolution. Unmatched URLs are returned unchanged.
func upgradeLastfmArtworkURL(value string) string {
	return strings.NewReplacer(
		"/avatar170s/", "/300x300/",
		"/174s/", "/300x300/",
		"/64s/", "/300x300/",
		"/300x300s/", "/300x300/",
	).Replace(value)
}
