package version

import (
	"strings"
	"testing"
)

func TestBinaryVersion_NotEmpty(t *testing.T) {
	if BinaryVersion == "" {
		t.Fatal("BinaryVersion must not be empty")
	}
}

func TestMinServerVersion_NotEmpty(t *testing.T) {
	if MinServerVersion == "" {
		t.Fatal("MinServerVersion must not be empty")
	}
}

// TestMinServerVersion_SemVerish is a sanity check — we don't run a
// full semver validation (callers handle that) but we want at least
// one `.` so a typo like "010" doesn't slip past compile.
func TestMinServerVersion_SemVerish(t *testing.T) {
	if !strings.Contains(MinServerVersion, ".") {
		t.Fatalf("MinServerVersion = %q does not look like semver", MinServerVersion)
	}
}

// TestBinaryVersion_DevDefault confirms the literal default the
// goreleaser ldflags override against. If the default changes,
// update this assertion too.
func TestBinaryVersion_DevDefault(t *testing.T) {
	if BinaryVersion != "0.1.0-dev" {
		t.Fatalf("BinaryVersion = %q; want 0.1.0-dev (the pre-release default goreleaser overrides)", BinaryVersion)
	}
}
