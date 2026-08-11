package version

import "testing"

// TestDefaults pins the values the rest of the repo relies on when no ldflags
// override is applied. Issue #21.
func TestDefaults(t *testing.T) {
	if Version != "0.0.0-dev" {
		t.Fatalf("Version default = %q, want %q", Version, "0.0.0-dev")
	}
	if Commit != "none" {
		t.Fatalf("Commit default = %q, want %q", Commit, "none")
	}
	if s := String(); s == "" {
		t.Fatal("String() must not be empty")
	}
}
