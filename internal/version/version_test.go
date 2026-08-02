package version

import "testing"

func TestDefaultVersion(t *testing.T) {
	if Version != "v0.2.1" {
		t.Fatalf("expected default version v0.2.1, got %q", Version)
	}
}
