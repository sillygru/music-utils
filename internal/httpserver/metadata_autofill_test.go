package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/metadata"
)

// autofillStubProvider answers every lookup with the same enrichment template,
// renamed to whatever was asked for, so the resolver's identity check passes.
type autofillStubProvider struct {
	mu     sync.Mutex
	asked  []string
	answer *db.Track
}

func (p *autofillStubProvider) Name() string { return "stub" }

func (p *autofillStubProvider) Lookup(_ context.Context, input metadata.Input) (*db.Track, error) {
	p.mu.Lock()
	p.asked = append(p.asked, input.TrackName)
	p.mu.Unlock()
	if p.answer == nil {
		return nil, metadata.ErrNotFound
	}
	track := *p.answer
	track.Name = input.TrackName
	track.ArtistName = input.ArtistName
	return &track, nil
}

func (p *autofillStubProvider) askedFor(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	count := 0
	for _, asked := range p.asked {
		if asked == name {
			count++
		}
	}
	return count
}

func newTestAutofill(t *testing.T, database *sql.DB, provider metadata.Provider) *metadataAutofill {
	t.Helper()
	return &metadataAutofill{
		database:    database,
		resolver:    metadata.NewResolver(provider),
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		idleGap:     10 * time.Millisecond,
		priorityTTL: time.Minute,
		priority:    make(map[string]metadataAutofillItem),
		wake:        make(chan struct{}, 1),
	}
}

func seedPendingTrack(t *testing.T, database *sql.DB, name, artist string) int64 {
	t.Helper()
	id, err := db.UpsertTrackMetadata(context.Background(), database, db.Track{
		Name: name, ArtistName: artist, Duration: 200,
	})
	if err != nil {
		t.Fatalf("seed pending track %q: %v", name, err)
	}
	return id
}

func loadTrack(t *testing.T, database *sql.DB, name, artist string) *db.Track {
	t.Helper()
	track, err := db.FindTrackMetadataExact(context.Background(), database, name, artist, "", 0)
	if err != nil {
		t.Fatalf("load track %q: %v", name, err)
	}
	return track
}

func TestMetadataAutofillSettlesPendingTracksWhileIdle(t *testing.T) {
	metadataDB, _ := testHTTPDatabases(t)
	seedPendingTrack(t, metadataDB, "Pending One", "Some Artist")

	provider := &autofillStubProvider{answer: &db.Track{Genre: "Rock", Year: 2003, MetadataSource: "itunes"}}
	autofill := newTestAutofill(t, metadataDB, provider)

	autofill.work(context.Background())

	track := loadTrack(t, metadataDB, "Pending One", "Some Artist")
	if !track.MetadataChecked {
		t.Fatal("idle sweep left the track unsettled")
	}
	if track.Genre != "Rock" || track.Year != 2003 {
		t.Fatalf("idle sweep did not enrich the track: %+v", track)
	}
}

func TestMetadataAutofillPausesWhileRequestsAreRecent(t *testing.T) {
	metadataDB, _ := testHTTPDatabases(t)
	seedPendingTrack(t, metadataDB, "Busy Server", "Some Artist")

	provider := &autofillStubProvider{answer: &db.Track{Genre: "Rock"}}
	autofill := newTestAutofill(t, metadataDB, provider)
	autofill.idleGap = time.Hour
	autofill.noteRequest()

	autofill.work(context.Background())

	if calls := provider.askedFor("Busy Server"); calls != 0 {
		t.Fatalf("sweep spent %d upstream lookup(s) while a request was recent", calls)
	}
	if track := loadTrack(t, metadataDB, "Busy Server", "Some Artist"); track.MetadataChecked {
		t.Fatal("sweep settled a track while a request was recent")
	}
}

func TestMetadataAutofillPrioritisesRequestedTrackDuringTraffic(t *testing.T) {
	metadataDB, _ := testHTTPDatabases(t)
	seedPendingTrack(t, metadataDB, "Older Track", "Some Artist")
	requestedID := seedPendingTrack(t, metadataDB, "Requested Track", "Some Artist")

	provider := &autofillStubProvider{answer: &db.Track{Genre: "Pop"}}
	autofill := newTestAutofill(t, metadataDB, provider)
	autofill.idleGap = time.Hour
	autofill.noteRequest()
	autofill.enqueue(requestedID, metadata.Input{TrackName: "Requested Track", ArtistName: "Some Artist", Duration: 200})

	autofill.work(context.Background())

	if calls := provider.askedFor("Requested Track"); calls != 1 {
		t.Fatalf("requested track was looked up %d time(s), want 1", calls)
	}
	if track := loadTrack(t, metadataDB, "Requested Track", "Some Artist"); !track.MetadataChecked || track.Genre != "Pop" {
		t.Fatalf("requested track was not settled: %+v", track)
	}
	if track := loadTrack(t, metadataDB, "Older Track", "Some Artist"); track.MetadataChecked {
		t.Fatal("the routine sweep ran while a request was recent")
	}
}

// A lookup the provider chain cannot answer is queued for the automatic
// backfill, so a song that only failed because a provider hiccuped comes back on
// its own while the instance is busy.
func TestGetMetadataPartialHitQueuesTrackForImmediateBackfill(t *testing.T) {
	metadataDB, _ := testHTTPDatabases(t)
	seedPendingTrack(t, metadataDB, "Unresolved Song", "Some Artist")

	stub := &autofillStubProvider{answer: &db.Track{Genre: "Pop", Year: 2021, MetadataSource: "itunes"}}
	autofill := newTestAutofill(t, metadataDB, stub)
	autofill.idleGap = time.Hour

	handler := getMetadataHandler(metadataDB, metadata.NewResolver(&countingMetadataProvider{}), testFallbackGuard(), true, autofill)
	response := performRequest(t, handler, "/api/metadata/get?track_name=Unresolved+Song&artist_name=Some+Artist")
	if response.Code != http.StatusOK {
		t.Fatalf("expected the cached partial row, got %d: %s", response.Code, response.Body.String())
	}

	autofill.work(context.Background())

	track := loadTrack(t, metadataDB, "Unresolved Song", "Some Artist")
	if !track.MetadataChecked || track.Genre != "Pop" {
		t.Fatalf("queued track was not backfilled immediately: %+v", track)
	}
}

// A track nothing knows about answers 404 with a hint, because the automatic
// backfill may still settle it.
func TestGetMetadataMissAsksTheClientToCheckAgain(t *testing.T) {
	metadataDB, _ := testHTTPDatabases(t)

	handler := getMetadataHandler(metadataDB, metadata.NewResolver(&countingMetadataProvider{}), testFallbackGuard(), true, nil)
	response := performRequest(t, handler, "/api/metadata/get?track_name=Nowhere+Song&artist_name=Nobody")
	if response.Code != http.StatusNotFound {
		t.Fatalf("expected a miss, got %d: %s", response.Code, response.Body.String())
	}
	var miss apiError
	if err := json.NewDecoder(response.Body).Decode(&miss); err != nil {
		t.Fatalf("decode miss: %v", err)
	}
	if miss.Message != trackNotFoundMessage {
		t.Fatalf("expected the retry hint %q, got %q", trackNotFoundMessage, miss.Message)
	}
}

// Probes and stats endpoints are polled by machines, so they must not keep the
// automatic backfill paused forever.
func TestMetadataAutofillIgnoresOperationalPaths(t *testing.T) {
	autofill := &metadataAutofill{idleGap: time.Minute, priority: make(map[string]metadataAutofillItem)}
	handler := autofill.trackActivity(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	for _, path := range []string{"/api/healthz", "/api/version", "/api/stats/metadata"} {
		response := performRequest(t, handler, path)
		if response.Code != http.StatusOK {
			t.Fatalf("%s: unexpected status %d", path, response.Code)
		}
	}
	autofill.mu.Lock()
	nudged := !autofill.lastRequest.IsZero()
	autofill.mu.Unlock()
	if nudged {
		t.Fatal("operational endpoints paused the automatic backfill")
	}

	performRequest(t, handler, "/api/metadata/get?track_name=Song")
	autofill.mu.Lock()
	nudged = !autofill.lastRequest.IsZero()
	autofill.mu.Unlock()
	if !nudged {
		t.Fatal("a real request did not pause the automatic backfill")
	}
}
