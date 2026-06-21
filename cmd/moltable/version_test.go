package main

import (
	"encoding/json"
	"strings"
	"testing"

	clversion "github.com/moltable/cli/internal/version"
)

func TestVersion_HumanFormat(t *testing.T) {
	t.Setenv("MOLTABLE_NO_UPDATE_CHECK", "1")
	code, stdout, _ := runCLI(t, "version")
	if code != 0 {
		t.Fatalf("exit = %d; want 0", code)
	}
	out := stdout.String()
	want := "moltable " + clversion.BinaryVersion + " (min API version: " + clversion.MinServerVersion + ")"
	if !strings.HasPrefix(out, want) {
		t.Fatalf("stdout = %q; want prefix %q", out, want)
	}
}

func TestVersion_JSON(t *testing.T) {
	t.Setenv("MOLTABLE_NO_UPDATE_CHECK", "1")
	code, stdout, _ := runCLI(t, "version", "--json")
	if code != 0 {
		t.Fatalf("exit = %d; want 0", code)
	}
	var got map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v\nstdout=%s", err, stdout.String())
	}
	if got["binary_version"] != clversion.BinaryVersion {
		t.Fatalf("binary_version = %q; want %q", got["binary_version"], clversion.BinaryVersion)
	}
	if got["min_server_version"] != clversion.MinServerVersion {
		t.Fatalf("min_server_version = %q; want %q", got["min_server_version"], clversion.MinServerVersion)
	}
}

// When ldflags inject BinaryCommit and BinaryBuildDate (the goreleaser
// path), `version --json` carries them and the human shape renders
// them in the parenthetical. Both paths must stay schema-stable: keys
// always present, empty when unset.
func TestVersion_IncludesCommitAndBuildDateWhenSet(t *testing.T) {
	t.Setenv("MOLTABLE_NO_UPDATE_CHECK", "1")
	prevCommit, prevDate := clversion.BinaryCommit, clversion.BinaryBuildDate
	clversion.BinaryCommit = "abc1234"
	clversion.BinaryBuildDate = "2026-06-19T12:00:00Z"
	t.Cleanup(func() {
		clversion.BinaryCommit = prevCommit
		clversion.BinaryBuildDate = prevDate
	})

	// Human
	code, stdout, _ := runCLI(t, "version")
	if code != 0 {
		t.Fatalf("exit = %d; want 0", code)
	}
	human := stdout.String()
	for _, want := range []string{"commit abc1234", "built 2026-06-19T12:00:00Z", "min API version: " + clversion.MinServerVersion} {
		if !strings.Contains(human, want) {
			t.Errorf("human stdout missing %q\ngot: %s", want, human)
		}
	}

	// JSON
	code, stdout, _ = runCLI(t, "version", "--json")
	if code != 0 {
		t.Fatalf("exit = %d; want 0", code)
	}
	var got map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v\nstdout=%s", err, stdout.String())
	}
	if got["commit"] != "abc1234" {
		t.Errorf("commit = %q; want abc1234", got["commit"])
	}
	if got["build_date"] != "2026-06-19T12:00:00Z" {
		t.Errorf("build_date = %q; want 2026-06-19T12:00:00Z", got["build_date"])
	}
}

// Schema stability: in a default `go build` (no ldflags), commit and
// build_date are empty STRINGS — not missing keys — so agent parsers
// don't crash on a sometimes-missing field.
func TestVersion_JSONSchemaStableWhenCommitMissing(t *testing.T) {
	t.Setenv("MOLTABLE_NO_UPDATE_CHECK", "1")
	prevCommit, prevDate := clversion.BinaryCommit, clversion.BinaryBuildDate
	clversion.BinaryCommit = ""
	clversion.BinaryBuildDate = ""
	t.Cleanup(func() {
		clversion.BinaryCommit = prevCommit
		clversion.BinaryBuildDate = prevDate
	})

	code, stdout, _ := runCLI(t, "version", "--json")
	if code != 0 {
		t.Fatalf("exit = %d; want 0", code)
	}
	var got map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, ok := got["commit"]; !ok {
		t.Error("commit key must be present (empty string) even when not set")
	}
	if _, ok := got["build_date"]; !ok {
		t.Error("build_date key must be present (empty string) even when not set")
	}
}

func TestVersion_JSONWithJQ(t *testing.T) {
	t.Setenv("MOLTABLE_NO_UPDATE_CHECK", "1")
	code, stdout, _ := runCLI(t, "version", "--json", "--jq", ".binary_version")
	if code != 0 {
		t.Fatalf("exit = %d; want 0", code)
	}
	got := strings.TrimSpace(stdout.String())
	want := `"` + clversion.BinaryVersion + `"`
	if got != want {
		t.Fatalf("stdout = %q; want %q", got, want)
	}
}
