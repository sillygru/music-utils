package version

import "testing"

func TestDefaultVersion(t *testing.T) {
	if Version != "v0.2.0" {
		t.Fatalf("expected default version v0.2.0, got %q", Version)
	}
}
