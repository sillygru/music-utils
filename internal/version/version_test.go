package version

import "testing"

func TestDefaultVersion(t *testing.T) {
	if Version != "v0.1.0" {
		t.Fatalf("expected default version v0.1.0, got %q", Version)
	}
}
