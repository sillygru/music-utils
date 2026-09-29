package jobs

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

type stubJob struct {
	name    string
	summary string
	flags   string
	ran     bool
}

func (s *stubJob) Name() string    { return s.name }
func (s *stubJob) Summary() string { return s.summary }
func (s *stubJob) Flags() string   { return s.flags }
func (s *stubJob) Run(context.Context, Options) error {
	s.ran = true
	return nil
}

// withRegistry swaps in an isolated registry so tests do not depend on the
// jobs registered by the package under test.
func withRegistry(t *testing.T, jobs ...Job) {
	t.Helper()
	registryMu.Lock()
	previous := registry
	registry = map[string]Job{}
	registryMu.Unlock()
	t.Cleanup(func() {
		registryMu.Lock()
		registry = previous
		registryMu.Unlock()
	})
	for _, job := range jobs {
		Register(job)
	}
}

func TestRegisterAndLookup(t *testing.T) {
	job := &stubJob{name: "alpha", summary: "does alpha"}
	withRegistry(t, job)

	found, err := Lookup("alpha")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if found.Name() != "alpha" {
		t.Fatalf("unexpected job %q", found.Name())
	}
}

func TestLookupUnknownJob(t *testing.T) {
	withRegistry(t, &stubJob{name: "alpha", summary: "s"})
	_, err := Lookup("nope")
	if err == nil {
		t.Fatal("expected an error for an unknown job")
	}
	if !errors.Is(err, ErrUnknownJob) {
		t.Fatalf("expected ErrUnknownJob, got %v", err)
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("expected the error to name the job, got %v", err)
	}
}

func TestAllIsSortedByName(t *testing.T) {
	withRegistry(t,
		&stubJob{name: "zebra", summary: "z"},
		&stubJob{name: "alpha", summary: "a"},
		&stubJob{name: "mango", summary: "m"},
	)
	names := make([]string, 0, 3)
	for _, job := range All() {
		names = append(names, job.Name())
	}
	want := []string{"alpha", "mango", "zebra"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, names)
		}
	}
}

func TestRegisterPanicsOnDuplicateName(t *testing.T) {
	withRegistry(t, &stubJob{name: "dupe", summary: "s"})
	defer func() {
		if recover() == nil {
			t.Fatal("expected Register to panic on a duplicate name")
		}
	}()
	Register(&stubJob{name: "dupe", summary: "other"})
}

func TestRegisterPanicsOnEmptyName(t *testing.T) {
	withRegistry(t)
	defer func() {
		if recover() == nil {
			t.Fatal("expected Register to panic on an empty name")
		}
	}()
	Register(&stubJob{name: "", summary: "s"})
}

func TestPrintListShowsEveryJob(t *testing.T) {
	withRegistry(t,
		&stubJob{name: "metadata-backfill", summary: "fetch missing metadata", flags: "-dry-run"},
		&stubJob{name: "lyrics-backfill", summary: "fetch missing lyrics", flags: ""},
	)
	var out bytes.Buffer
	PrintList(&out)
	text := out.String()

	for _, want := range []string{
		"Available jobs (2)",
		"metadata-backfill",
		"fetch missing metadata",
		"-dry-run",
		"lyrics-backfill",
		"--run-job",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("expected listing to contain %q, got:\n%s", want, text)
		}
	}
	// A job without flags should not print an empty flags line.
	if strings.Contains(text, "flags: \n") {
		t.Errorf("expected no empty flags line, got:\n%s", text)
	}
}

func TestPrintListWithNoJobs(t *testing.T) {
	withRegistry(t)
	var out bytes.Buffer
	PrintList(&out)
	if !strings.Contains(out.String(), "No jobs") {
		t.Fatalf("expected an empty-registry message, got %q", out.String())
	}
}

func TestRegisteredMetadataBackfillIsDiscoverable(t *testing.T) {
	// The real job registers itself at init; guard against it being renamed or
	// dropped, since the documented CLI contract depends on the name.
	job, err := Lookup("metadata-backfill")
	if err != nil {
		t.Fatalf("metadata-backfill must be registered: %v", err)
	}
	if job.Summary() == "" {
		t.Error("expected a non-empty summary for --jobs output")
	}
}
