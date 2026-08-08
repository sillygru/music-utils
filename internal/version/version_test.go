package version

import "testing"

func TestDefaultVersion(t *testing.T) {
	if Version != "v0.4.0" {
		t.Fatalf("expected default version v0.4.0, got %q", Version)
	}
}
