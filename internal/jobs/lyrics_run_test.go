package jobs

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sillygru/music-utils/internal/config"
	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/lrclib"
	"github.com/sillygru/music-utils/internal/upstream"
)

// jobDatabases opens a migrated metadata and lyrics pair for a whole-run test.
//
// These are real files rather than :memory:. A run has the producer, the slots and
// the writer all reaching the databases at once, and an in-memory SQLite handle
// gives every connection a private database of its own unless a shared cache is
// asked for, so two connections would each see half the work.
func jobDatabases(t *testing.T) (*sql.DB, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	open := func(label string) *sql.DB {
		database, err := db.Open(filepath.Join(dir, label+".db"), db.Config{
			MmapSize:        512 * 1024 * 1024,
			CacheSizeKB:     -64000,
			MaxOpenConns:    2,
			TxLockImmediate: true,
		})
		if err != nil {
			t.Fatalf("open %s: %v", label, err)
		}
		return database
	}
	metadataDB, lyricsDB := open("metadata"), open("lyrics")
	t.Cleanup(func() { _ = metadataDB.Close(); _ = lyricsDB.Close() })

	ctx := context.Background()
	if err := db.MigrateMetadata(ctx, metadataDB); err != nil {
		t.Fatalf("migrate metadata: %v", err)
	}
	if err := db.MigrateLyrics(ctx, lyricsDB); err != nil {
		t.Fatalf("migrate lyrics: %v", err)
	}
	if err := db.EnsureCoordination(ctx, metadataDB); err != nil {
		t.Fatalf("ensure coordination: %v", err)
	}
	return metadataDB, lyricsDB
}

// seedTrack inserts an unsettled track with a complete identity, which is what the
// job treats as safe to record a negative against.
//
// The row is written the way the request path writes one, lowercased keys included,
// so the lookups the run and the tests perform find it the same way they would find
// a real library row.
func seedTrack(t *testing.T, metadataDB *sql.DB, name string) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := db.UpsertTrackMetadata(ctx, metadataDB, db.Track{
		Name:       name,
		ArtistName: "Artist",
		AlbumName:  "Album",
		Duration:   200,
	}); err != nil {
		t.Fatalf("seed track %q: %v", name, err)
	}
	var id int64
	if err := metadataDB.QueryRowContext(ctx,
		"SELECT id FROM tracks WHERE name_lower = ?", strings.ToLower(name)).Scan(&id); err != nil {
		t.Fatalf("read track id for %q: %v", name, err)
	}
	return id
}

// runJob drives a whole run through the real producer, slots and writer against
// real databases, with only the upstream replaced.
func runJob(t *testing.T, metadataDB, lyricsDB *sql.DB, resolver *lyricsBackfillResolver, planned int64, mutate func(*lyricsBackfillRun)) *lyricsBackfillRun { //nolint:revive
	t.Helper()
	lanes := providerLanes(resolver)
	run := &lyricsBackfillRun{
		opts: Options{
			MetadataDB:     metadataDB,
			LyricsDB:       lyricsDB,
			LyricsDBPath:   ":memory:",
			Config:         config.Config{},
			MaxWriteErrors: 5,
		},
		resolver:       resolver,
		gate:           NewGate(0),
		progress:       NewProgress(io.Discard, lanes),
		lanes:          lanes,
		slots:          len(lanes),
		maxWriteErrors: 5,
		writeAbort:     make(chan struct{}),
		writeOK:        make(chan struct{}, 1),
		watchDone:      make(chan struct{}),
	}
	if mutate != nil {
		mutate(run)
	}
	if err := run.execute(context.Background(), planned); err != nil {
		t.Fatalf("run: %v", err)
	}
	return run
}

// A whole run has to leave the library in the state it promises: every song
// settled, every provider's answer stored and recorded, and the pointer on the best
// one. This is the only test that walks the producer, the slots and the writer
// together, so it is the one that would catch the parts disagreeing with each other.
func TestWholeRunAsksEveryProviderAboutEverySongAndSettlesIt(t *testing.T) {
	const (
		providers = 4
		songs     = 6
	)
	metadataDB, lyricsDB := jobDatabases(t)
	names := make([]string, songs)
	for i := range songs {
		names[i] = fmt.Sprintf("Song %d", i)
		seedTrack(t, metadataDB, names[i])
	}

	var mu sync.Mutex
	asked := map[string]map[string]int{}
	resolver := countingResolver(t, providers, func(provider string, work db.LyricsWork) (*db.Lyrics, error) {
		mu.Lock()
		if asked[work.Name] == nil {
			asked[work.Name] = map[string]int{}
		}
		asked[work.Name][provider]++
		mu.Unlock()
		// Synced from one provider and plain from another, so the pointer has to be
		// chosen on content rather than on which answered first.
		switch provider {
		case "p1":
			return &db.Lyrics{PlainLyrics: "words for " + work.Name}, nil
		case "p2":
			return &db.Lyrics{PlainLyrics: "words", SyncedLyrics: "[00:01.00]timed"}, nil
		}
		return nil, lrclib.ErrNotFound
	})

	runJob(t, metadataDB, lyricsDB, resolver, songs, nil)

	mu.Lock()
	if len(asked) != songs {
		t.Errorf("%d songs were asked about, want %d", len(asked), songs)
	}
	for _, name := range names {
		if len(asked[name]) != providers {
			t.Errorf("%q was asked of %d providers, want %d", name, len(asked[name]), providers)
		}
		for provider, count := range asked[name] {
			if count != 1 {
				t.Errorf("%q asked %q %d times, want 1", name, provider, count)
			}
		}
	}
	mu.Unlock()

	ctx := context.Background()
	pending, err := db.CountTracksMissingLyrics(ctx, metadataDB)
	if err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending != 0 {
		t.Errorf("%d songs still unsettled after a complete run", pending)
	}

	// Every provider's answer is in the ledger, successes and misses alike, which
	// is what stops the request path re-asking any of them for these songs.
	fetches, err := db.ListProviderFetchesForTracks(ctx, lyricsDB, trackIDs(t, metadataDB, names))
	if err != nil {
		t.Fatalf("list provider fetches: %v", err)
	}
	if len(fetches) != songs {
		t.Fatalf("%d songs have ledger rows, want %d", len(fetches), songs)
	}
	providerNames := resolver.names()
	for _, name := range names {
		for _, pname := range providerNames {
			if _, ok := fetches[trackIDOf(t, metadataDB, name)][pname]; !ok {
				t.Errorf("%q has no ledger row for %q", name, pname)
			}
		}
	}

	// The served answer is the synced one, for every song, even though a plain-only
	// provider was in the chain too.
	for _, name := range names {
		id := trackIDOf(t, metadataDB, name)
		var served int64
		if err := metadataDB.QueryRowContext(ctx, "SELECT last_lyrics_id FROM tracks WHERE id=?", id).Scan(&served); err != nil {
			t.Fatalf("read last_lyrics_id: %v", err)
		}
		if served == 0 {
			t.Errorf("%q has no served lyrics after a complete run", name)
			continue
		}
		var synced bool
		if err := lyricsDB.QueryRowContext(ctx, "SELECT has_synced_lyrics FROM lyrics WHERE id=?", served).Scan(&synced); err != nil {
			t.Fatalf("read served row: %v", err)
		}
		if !synced {
			t.Errorf("%q serves a plain-only answer; the synced one should have won", name)
		}
	}
}

// A song every provider misses is still settled. Recording it is what stops the run
// asking the same question of all four providers on every pass forever.
func TestWholeRunSettlesASongNoProviderHasLyricsFor(t *testing.T) {
	metadataDB, lyricsDB := jobDatabases(t)
	seedTrack(t, metadataDB, "Obscure")

	resolver := countingResolver(t, 3, func(string, db.LyricsWork) (*db.Lyrics, error) {
		return nil, lrclib.ErrNotFound
	})
	runJob(t, metadataDB, lyricsDB, resolver, 1, nil)

	pending, err := db.CountTracksMissingLyrics(context.Background(), metadataDB)
	if err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending != 0 {
		t.Errorf("%d songs still unsettled, want a confirmed miss to settle the song", pending)
	}
}

// The resume is what makes an interrupted run cheap. A song the ledger already
// covers for some providers must not be asked about those again, and it must still
// settle once the rest have answered.
func TestWholeRunResumesAPartlyAnsweredSong(t *testing.T) {
	const providers = 4
	metadataDB, lyricsDB := jobDatabases(t)
	id := seedTrack(t, metadataDB, "Partly Done")
	ctx := context.Background()

	// Stand in for a previous run that got two providers in before it stopped: one
	// that found lyrics and one that gave a definitive miss. Both have been asked,
	// so neither may be asked again, which is why the ledger's success flag is not
	// what decides this.
	for _, provider := range []string{"p0", "p1"} {
		success := provider == "p0"
		if err := db.UpsertProviderFetch(ctx, lyricsDB, id, provider, success); err != nil {
			t.Fatalf("record %s: %v", provider, err)
		}
	}

	var mu sync.Mutex
	calls := map[string]int{}
	resolver := countingResolver(t, providers, func(provider string, _ db.LyricsWork) (*db.Lyrics, error) {
		mu.Lock()
		calls[provider]++
		mu.Unlock()
		return &db.Lyrics{PlainLyrics: "words"}, nil
	})

	run := runJob(t, metadataDB, lyricsDB, resolver, 1, nil)

	mu.Lock()
	for _, provider := range []string{"p0", "p1"} {
		if calls[provider] != 0 {
			t.Errorf("%q was asked again for a song the ledger already covers (%d calls)", provider, calls[provider])
		}
	}
	for _, provider := range []string{"p2", "p3"} {
		if calls[provider] != 1 {
			t.Errorf("%q was asked %d times, want 1: only the missing providers should be asked", provider, calls[provider])
		}
	}
	mu.Unlock()

	pending, err := db.CountTracksMissingLyrics(ctx, metadataDB)
	if err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending != 0 {
		t.Errorf("%d songs still unsettled after the resume", pending)
	}
	run.mu.Lock()
	resumed := run.resumed
	run.mu.Unlock()
	if resumed != 1 {
		t.Errorf("resumed = %d, want 1 so the summary reports the carried-over work", resumed)
	}
}

// A refresh is the operator asking again on purpose, so it must re-ask providers the
// ledger already covers. Reading it here would quietly turn -refresh into a no-op.
func TestWholeRunRefreshIgnoresTheLedger(t *testing.T) {
	const providers = 3
	metadataDB, lyricsDB := jobDatabases(t)
	id := seedTrack(t, metadataDB, "Refreshed")
	ctx := context.Background()
	if err := db.UpsertProviderFetch(ctx, lyricsDB, id, "p0", false); err != nil {
		t.Fatalf("record p0: %v", err)
	}
	// Put it in the settled set so it is only reachable through the refresh phase.
	if _, err := metadataDB.ExecContext(ctx,
		"UPDATE tracks SET lyrics_checked=1, lyrics_checked_at=? WHERE id=?",
		time.Now().Add(-72*time.Hour).UTC().Format("2006-01-02 15:04:05"), id); err != nil {
		t.Fatalf("age the track: %v", err)
	}

	var mu sync.Mutex
	calls := map[string]int{}
	resolver := countingResolver(t, providers, func(provider string, _ db.LyricsWork) (*db.Lyrics, error) {
		mu.Lock()
		calls[provider]++
		mu.Unlock()
		return &db.Lyrics{PlainLyrics: "words"}, nil
	})

	runJob(t, metadataDB, lyricsDB, resolver, 1, func(run *lyricsBackfillRun) {
		run.opts.Refresh = true
		run.selection = refreshSelection{enabled: true, olderThan: 24 * time.Hour}
	})

	mu.Lock()
	defer mu.Unlock()
	for provider, count := range calls {
		if count != 1 {
			t.Errorf("%q was asked %d times, want 1: a refresh re-asks everyone", provider, count)
		}
	}
	if len(calls) != providers {
		t.Errorf("%d providers were asked, want all %d", len(calls), providers)
	}
}

// A refresh must re-ask providers even when a track already has usable lyrics cached,
// because finding better or updated lyrics is the whole purpose of -refresh.
func TestWholeRunRefreshReasksTracksWithCachedLyrics(t *testing.T) {
	const providers = 2
	metadataDB, lyricsDB := jobDatabases(t)
	ctx := context.Background()

	trackID, _, err := db.InsertTrackWithLyrics(ctx, metadataDB, lyricsDB,
		db.Track{Name: "AlreadyCached", ArtistName: "Artist", AlbumName: "Album", Duration: 180},
		db.Lyrics{PlainLyrics: "old plain lyrics"})
	if err != nil {
		t.Fatalf("insert cached track: %v", err)
	}

	// Age the track so it falls into the stale refresh set.
	if _, err := metadataDB.ExecContext(ctx,
		"UPDATE tracks SET lyrics_checked=1, lyrics_checked_at=? WHERE id=?",
		time.Now().Add(-72*time.Hour).UTC().Format("2006-01-02 15:04:05"), trackID); err != nil {
		t.Fatalf("age the track: %v", err)
	}

	var mu sync.Mutex
	calls := map[string]int{}
	resolver := countingResolver(t, providers, func(provider string, _ db.LyricsWork) (*db.Lyrics, error) {
		mu.Lock()
		calls[provider]++
		mu.Unlock()
		return &db.Lyrics{PlainLyrics: "new plain lyrics", SyncedLyrics: "[00:01.00]synced"}, nil
	})

	runJob(t, metadataDB, lyricsDB, resolver, 1, func(run *lyricsBackfillRun) {
		run.opts.Refresh = true
		run.selection = refreshSelection{enabled: true, olderThan: 24 * time.Hour}
	})

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != providers {
		t.Errorf("asked %d providers on refresh, want all %d (cached lyrics must not bypass refresh)", len(calls), providers)
	}
}

// A provider that stays rate limited must leave the song unsettled rather than
// settled on the strength of the providers that did answer. That is the difference
// between "we checked" and "one upstream was briefly unhappy".
func TestWholeRunLeavesASongUnsettledWhenAProviderNeverAnswers(t *testing.T) {
	metadataDB, lyricsDB := jobDatabases(t)
	seedTrack(t, metadataDB, "Throttled")

	resolver := countingResolver(t, 2, func(provider string, _ db.LyricsWork) (*db.Lyrics, error) {
		if provider == "p0" {
			return nil, &upstream.RateLimitError{
				Provider:   provider,
				Status:     http.StatusTooManyRequests,
				RetryAfter: time.Millisecond,
			}
		}
		return &db.Lyrics{PlainLyrics: "words"}, nil
	})

	runJob(t, metadataDB, lyricsDB, resolver, 1, nil)

	pending, err := db.CountTracksMissingLyrics(context.Background(), metadataDB)
	if err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending != 1 {
		t.Errorf("%d songs still unsettled, want the throttled one left for the next run", pending)
	}
}

// The answers a song did get are on disk even though the song was not settled, which
// is the point of writing as each answer arrives rather than batching to the end.
func TestWholeRunKeepsTheAnswersItGotBeforeGivingUp(t *testing.T) {
	metadataDB, lyricsDB := jobDatabases(t)
	id := seedTrack(t, metadataDB, "Partial")

	resolver := countingResolver(t, 2, func(provider string, _ db.LyricsWork) (*db.Lyrics, error) {
		if provider == "p0" {
			return nil, &upstream.RateLimitError{Provider: provider, Status: http.StatusTooManyRequests}
		}
		return &db.Lyrics{PlainLyrics: "words"}, nil
	})
	runJob(t, metadataDB, lyricsDB, resolver, 1, nil)

	// The request path finds lyrics only through the pointer, so a song that is not
	// settled still has to be playable if an answer came back.
	_, lyrics, err := db.FindTrackExact(context.Background(), metadataDB, lyricsDB, "Partial", "Artist", "Album", 0)
	if err != nil {
		t.Fatalf("find track exact: %v", err)
	}
	if lyrics.PlainLyrics != "words" {
		t.Errorf("plain lyrics = %q, want the answer that did arrive to be usable", lyrics.PlainLyrics)
	}
	if id == 0 {
		t.Fatal("no track id")
	}
}

// A hard failure is not a miss. Recording it as one would tell the request path the
// song has no lyrics when in fact nobody managed to ask.
func TestWholeRunDoesNotRecordAFailureAsAMiss(t *testing.T) {
	metadataDB, lyricsDB := jobDatabases(t)
	id := seedTrack(t, metadataDB, "Broken")

	resolver := countingResolver(t, 2, func(provider string, _ db.LyricsWork) (*db.Lyrics, error) {
		if provider == "p0" {
			return nil, errors.New("dial tcp: connection refused")
		}
		return nil, lrclib.ErrNotFound
	})
	runJob(t, metadataDB, lyricsDB, resolver, 1, nil)

	ctx := context.Background()
	fetches, err := db.ListProviderFetchesForTracks(ctx, lyricsDB, []int64{id})
	if err != nil {
		t.Fatalf("list provider fetches: %v", err)
	}
	if _, ok := fetches[id]["p0"]; ok {
		t.Error("a transport failure was recorded in the ledger as though the provider had answered")
	}
	if _, ok := fetches[id]["p1"]; !ok {
		t.Error("a definitive miss should still be recorded, so the request path does not re-ask it")
	}

	pending, err := db.CountTracksMissingLyrics(ctx, metadataDB)
	if err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending != 1 {
		t.Errorf("got %d pending tracks, want 1: a transport failure must leave the song unsettled", pending)
	}
}

// A song whose lyrics are already cached must not cost a single upstream request,
// which is what lets a first run over an already-whole library finish cheaply.
func TestWholeRunSettlesACachedSongWithoutAskingAnyone(t *testing.T) {
	metadataDB, lyricsDB := jobDatabases(t)
	ctx := context.Background()
	if _, _, err := db.InsertTrackWithLyrics(ctx, metadataDB, lyricsDB,
		db.Track{Name: "Cached", ArtistName: "Artist", AlbumName: "Album"},
		db.Lyrics{PlainLyrics: "already here"}); err != nil {
		t.Fatalf("seed cached track: %v", err)
	}
	// Clear the flag so the job treats it as unsettled work.
	var cachedID int64
	if err := metadataDB.QueryRowContext(ctx, "SELECT id FROM tracks WHERE name_lower='cached'").Scan(&cachedID); err != nil {
		t.Fatalf("read id: %v", err)
	}
	if _, err := metadataDB.ExecContext(ctx, "UPDATE tracks SET lyrics_checked=0 WHERE id=?", cachedID); err != nil {
		t.Fatalf("clear the flag: %v", err)
	}

	var mu sync.Mutex
	calls := 0
	resolver := countingResolver(t, 3, func(string, db.LyricsWork) (*db.Lyrics, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return &db.Lyrics{PlainLyrics: "words"}, nil
	})

	runJob(t, metadataDB, lyricsDB, resolver, 1, nil)

	mu.Lock()
	if calls != 0 {
		t.Errorf("%d upstream requests for a cached song, want 0", calls)
	}
	mu.Unlock()

	pending, err := db.CountTracksMissingLyrics(ctx, metadataDB)
	if err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending != 0 {
		t.Errorf("%d songs still unsettled, want a cached song settled with no upstream request", pending)
	}
}

func trackIDs(t *testing.T, metadataDB *sql.DB, names []string) []int64 {
	t.Helper()
	ids := make([]int64, 0, len(names))
	for _, name := range names {
		ids = append(ids, trackIDOf(t, metadataDB, name))
	}
	return ids
}

func trackIDOf(t *testing.T, metadataDB *sql.DB, name string) int64 {
	t.Helper()
	var id int64
	if err := metadataDB.QueryRowContext(context.Background(),
		"SELECT id FROM tracks WHERE name_lower=?", strings.ToLower(name)).Scan(&id); err != nil {
		t.Fatalf("read track id for %q: %v", name, err)
	}
	return id
}

// headerSignal is an io.Writer that hands back what a run printed and reports the
// moment a line it cares about appears. Signalling on the write rather than
// sleeping keeps the test off the clock: a run paced at one request every two
// seconds would otherwise need two seconds of wall time to prove a line was
// already on disk.
type headerSignal struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	needle  string
	seen    chan struct{}
	closed  bool
	release func()
}

func newHeaderSignal(needle string, release func()) *headerSignal {
	return &headerSignal{needle: needle, seen: make(chan struct{}), release: release}
}

func (h *headerSignal) Write(p []byte) (int, error) {
	h.mu.Lock()
	h.buf.Write(p)
	found := strings.Contains(h.buf.String(), h.needle)
	if found && !h.closed {
		h.closed = true
		// Released outside the lock: the run continues on this goroutine and the
		// next thing it does is wait on the pacer, which is the sleep this is
		// avoiding.
		go h.release()
	}
	h.mu.Unlock()
	return len(p), nil
}

func (h *headerSignal) String() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.buf.String()
}

// The run header is the only statement an operator gets of the rate the job is
// held to, and a slow pace has no whole req/s value to round to. A job paced at
// one request every two seconds used to render as "1 req/s", which is twice the
// rate it was allowed to make.
func TestLyricsBackfillHeaderStatesTheConfiguredJobPace(t *testing.T) {
	metadataDB, lyricsDB := jobDatabases(t)
	seedTrack(t, metadataDB, "Header Pace")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	cfg := config.Config{
		LRCLIBFallbackEnabled:   true,
		LRCLIBBaseURL:           upstream.URL + "/api",
		LRCLIBUserAgent:         "music-utils/test",
		LRCLIBTimeoutMS:         1000,
		UpstreamLyricsJobPaceMS: 2000,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signal := newHeaderSignal("per provider", cancel)

	job := &lyricsBackfill{}
	_ = job.Run(ctx, Options{
		Config:         cfg,
		MetadataDB:     metadataDB,
		LyricsDB:       lyricsDB,
		MetadataDBPath: ":memory:",
		LyricsDBPath:   ":memory:",
		Out:            signal,
		ErrOut:         io.Discard,
		Concurrency:    1,
		Limit:          1,
		DryRun:         true,
		MaxWriteErrors: 5,
	})

	header := signal.String()
	if !strings.Contains(header, "1 req/2s") {
		t.Fatalf("expected the header to state the 2s job pace as 1 req/2s, got:\n%s", header)
	}
	if strings.Contains(header, "1 req/s") {
		t.Fatalf("header claims 1 req/s for a run paced at one request per 2s:\n%s", header)
	}
}
