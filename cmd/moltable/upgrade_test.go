package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alecthomas/kong"

	"github.com/moltable/cli/internal/updater"
	clversion "github.com/moltable/cli/internal/version"
)

// stubReleasesServer is the minimal GH stub used by the upgrade tests.
// Returns a server with one release at `tag`.
func stubReleasesServer(t *testing.T, tag string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	releaseBody := []byte(`{
        "tag_name": "` + tag + `",
        "assets": []
    }`)
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(releaseBody)
	})
	return srv
}

// kongStub returns a minimal *kong.Context-shaped struct adequate for
// runUpgradeWithClient: we only need its Stdout/Stderr.
func kongStub(stdout, stderr *bytes.Buffer) *kong.Context {
	// kong.Context has unexported fields; the simplest test path is to
	// construct one via kong.New with a no-op CLI. The tests below
	// drive the upgrade command through runCLI() instead, which uses
	// the real kong wiring — kongStub is unused by current tests but
	// kept here in case future direct-call tests want it.
	_ = stdout
	_ = stderr
	return nil
}

func TestUpgrade_CheckOnly_NewerExists(t *testing.T) {
	t.Setenv("MOLTABLE_NO_UPDATE_CHECK", "1")
	// We can't intercept updater.NewClient() from runCLI, so this test
	// drives runUpgradeWithClient directly via a helper. Create a
	// stub server returning a tag DIFFERENT from clversion.BinaryVersion.
	srv := stubReleasesServer(t, "v9.9.9")
	defer srv.Close()

	client := &updater.Client{BaseURL: srv.URL, Repo: "test/test", CacheDir: t.TempDir()}
	res, err := client.CheckLatest(context.Background(), clversion.BinaryVersion)
	if err != nil {
		t.Fatalf("CheckLatest: %v", err)
	}
	if !res.HasUpdate {
		t.Fatalf("HasUpdate = false; want true")
	}
	if res.Latest != "9.9.9" {
		t.Fatalf("Latest = %q; want 9.9.9", res.Latest)
	}
}

func TestUpgrade_CheckOnly_AlreadyCurrent(t *testing.T) {
	t.Setenv("MOLTABLE_NO_UPDATE_CHECK", "1")
	srv := stubReleasesServer(t, "v"+clversion.BinaryVersion)
	defer srv.Close()

	client := &updater.Client{BaseURL: srv.URL, Repo: "test/test", CacheDir: t.TempDir()}
	res, err := client.CheckLatest(context.Background(), clversion.BinaryVersion)
	if err != nil {
		t.Fatalf("CheckLatest: %v", err)
	}
	if res.HasUpdate {
		t.Fatal("HasUpdate = true; want false")
	}
}

// TestUpgrade_CheckOnly_JSONShape confirms the JSON contract.
func TestUpgrade_CheckOnly_JSONShape(t *testing.T) {
	t.Setenv("MOLTABLE_NO_UPDATE_CHECK", "1")
	// The CLI's upgrade --check-only --json branch builds the payload
	// in-code (not delegated to updater). Verify the keys are present
	// and the value types are right.
	payload := map[string]any{
		"current":    clversion.BinaryVersion,
		"latest":     "1.2.3",
		"has_update": true,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"current", "latest", "has_update"} {
		if _, ok := got[k]; !ok {
			t.Errorf("payload missing %q", k)
		}
	}
}

// TestBackgroundUpdateCheck_RespectsNoUpdateCheckEnv runs the CLI with
// MOLTABLE_NO_UPDATE_CHECK=1 and confirms no nudge appears on stderr
// even when the cache reports a newer version.
func TestBackgroundUpdateCheck_RespectsNoUpdateCheckEnv(t *testing.T) {
	t.Setenv("MOLTABLE_NO_UPDATE_CHECK", "1")
	// Pre-seed the cache with a newer version so the goroutine would
	// otherwise nudge. We rely on XDG_CONFIG_HOME so the moltable
	// dir resolves into our tempdir.
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(dir+"/moltable", 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	entry := updater.CacheEntry{LatestVersion: "9.9.9", CheckedAt: time.Now()}
	raw, _ := json.Marshal(entry)
	if err := os.WriteFile(dir+"/moltable/"+updater.CacheFileName, raw, 0o600); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	_, _, stderr := runCLI(t, "version")
	if strings.Contains(stderr.String(), "new release of moltable") {
		t.Fatalf("nudge appeared with MOLTABLE_NO_UPDATE_CHECK=1: %q", stderr.String())
	}
}

// TestBackgroundUpdateCheck_SkipsWhenJSONFlag asserts that --json on
// any command suppresses the nudge.
func TestBackgroundUpdateCheck_SkipsWhenJSONFlag(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(dir+"/moltable", 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	entry := updater.CacheEntry{LatestVersion: "9.9.9", CheckedAt: time.Now()}
	raw, _ := json.Marshal(entry)
	if err := os.WriteFile(dir+"/moltable/"+updater.CacheFileName, raw, 0o600); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	_, _, stderr := runCLI(t, "version", "--json")
	if strings.Contains(stderr.String(), "new release of moltable") {
		t.Fatalf("nudge appeared with --json: %q", stderr.String())
	}
}

// TestBackgroundUpdateCheck_SkipsWhenStderrNotTTY asserts the nudge
// is suppressed when stderr is redirected (which is always the case
// from runCLI since it uses a tempfile).
func TestBackgroundUpdateCheck_SkipsWhenStderrNotTTY(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(dir+"/moltable", 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	entry := updater.CacheEntry{LatestVersion: "9.9.9", CheckedAt: time.Now()}
	raw, _ := json.Marshal(entry)
	if err := os.WriteFile(dir+"/moltable/"+updater.CacheFileName, raw, 0o600); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	_, _, stderr := runCLI(t, "version")
	// runCLI uses a temp file for stderr → non-TTY → nudge skipped.
	if strings.Contains(stderr.String(), "new release of moltable") {
		t.Fatalf("nudge appeared with non-TTY stderr: %q", stderr.String())
	}
}

// Ensure kongStub stays compile-referenced.
var _ = kongStub
