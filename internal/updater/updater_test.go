package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// makeTarballWithBinary returns a gzipped tar archive that contains a
// single regular file named `moltable` with the given payload.
func makeTarballWithBinary(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{
		Name: "moltable",
		Mode: 0o755,
		Size: int64(len(payload)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// stubGitHubServer mounts handlers for both the GitHub API release
// endpoint and the asset download URLs, so tests run against one
// httptest.Server.
func stubGitHubServer(t *testing.T, tag string, tarball []byte, checksums string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)

	// Use deferred late-binding of srv.URL inside handlers because the
	// asset browser_download_urls must point back at the same test server.
	releasePayload := func() []byte {
		resp := ghReleaseResponse{
			TagName: tag,
			Assets: []ghReleaseAsset{
				{
					Name:               fmt.Sprintf("moltable_%s_linux_amd64.tar.gz", stripVersionPrefix(tag)),
					BrowserDownloadURL: srv.URL + "/dl/tarball",
				},
				{
					Name:               "checksums.txt",
					BrowserDownloadURL: srv.URL + "/dl/checksums",
				},
			},
		}
		raw, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("marshal release: %v", err)
		}
		return raw
	}

	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		// Matches both /releases/latest and /releases/tags/<tag>
		if strings.Contains(r.URL.Path, "/releases/latest") || strings.Contains(r.URL.Path, "/releases/tags/") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(releasePayload())
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/dl/tarball", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarball)
	})
	mux.HandleFunc("/dl/checksums", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(checksums))
	})
	return srv
}

func TestCheckLatest_FreshCacheSkipsNetwork(t *testing.T) {
	dir := t.TempDir()
	// Pre-seed cache with a recent entry.
	entry := CacheEntry{LatestVersion: "0.9.0", CheckedAt: time.Now()}
	raw, _ := json.Marshal(entry)
	if err := os.WriteFile(filepath.Join(dir, CacheFileName), raw, 0o600); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	// Server that should NEVER be hit. Any request → fail the test.
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(500)
	}))
	defer srv.Close()

	c := &Client{
		BaseURL:  srv.URL,
		Repo:     "test/test",
		CacheDir: dir,
		Now:      func() time.Time { return entry.CheckedAt.Add(time.Hour) }, // 1h after cache write
	}
	res, err := c.CheckLatest(context.Background(), "0.1.0")
	if err != nil {
		t.Fatalf("CheckLatest: %v", err)
	}
	if calls != 0 {
		t.Fatalf("network calls = %d; want 0 (fresh cache)", calls)
	}
	if res.Latest != "0.9.0" {
		t.Fatalf("Latest = %q; want 0.9.0", res.Latest)
	}
	if !res.HasUpdate {
		t.Fatalf("HasUpdate = false; want true (0.9.0 > 0.1.0)")
	}
}

func TestCheckLatest_StaleCacheFetchesAndWrites(t *testing.T) {
	dir := t.TempDir()
	// Stale cache: 25h ago (CacheTTL is 24h).
	staleEntry := CacheEntry{LatestVersion: "0.1.0", CheckedAt: time.Now().Add(-25 * time.Hour)}
	raw, _ := json.Marshal(staleEntry)
	if err := os.WriteFile(filepath.Join(dir, CacheFileName), raw, 0o600); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	srv := stubGitHubServer(t, "v0.5.0", nil, "")
	defer srv.Close()

	c := &Client{
		BaseURL:  srv.URL,
		Repo:     "test/test",
		CacheDir: dir,
		Now:      time.Now,
	}
	res, err := c.CheckLatest(context.Background(), "0.1.0")
	if err != nil {
		t.Fatalf("CheckLatest: %v", err)
	}
	if res.Latest != "0.5.0" {
		t.Fatalf("Latest = %q; want 0.5.0", res.Latest)
	}

	// Cache should now contain the new value.
	raw, err = os.ReadFile(filepath.Join(dir, CacheFileName))
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	var updated CacheEntry
	if err := json.Unmarshal(raw, &updated); err != nil {
		t.Fatalf("parse cache: %v", err)
	}
	if updated.LatestVersion != "0.5.0" {
		t.Fatalf("cache LatestVersion = %q; want 0.5.0", updated.LatestVersion)
	}
}

func TestCheckLatest_NoCacheFetches(t *testing.T) {
	dir := t.TempDir()
	srv := stubGitHubServer(t, "v0.5.0", nil, "")
	defer srv.Close()

	c := &Client{
		BaseURL:  srv.URL,
		Repo:     "test/test",
		CacheDir: dir,
	}
	res, err := c.CheckLatest(context.Background(), "0.5.0")
	if err != nil {
		t.Fatalf("CheckLatest: %v", err)
	}
	if res.Latest != "0.5.0" {
		t.Fatalf("Latest = %q; want 0.5.0", res.Latest)
	}
	if res.HasUpdate {
		t.Fatal("HasUpdate = true; want false (server matches current)")
	}
}

// Regression: tag of shape `vX.Y.Z` (the monorepo's CLI release
// prefix) must normalize to `X.Y.Z` end-to-end — both CheckLatest and
// Apply rely on the resulting string for version comparison and asset
// name construction.
func TestCheckLatest_StripsCliVPrefix(t *testing.T) {
	srv := stubGitHubServer(t, "v0.5.0", nil, "")
	defer srv.Close()

	c := &Client{
		BaseURL:  srv.URL,
		Repo:     "test/test",
		CacheDir: t.TempDir(),
	}
	res, err := c.CheckLatest(context.Background(), "0.5.0")
	if err != nil {
		t.Fatalf("CheckLatest: %v", err)
	}
	if res.Latest != "0.5.0" {
		t.Fatalf("Latest = %q; want 0.5.0 (cli- prefix should be stripped)", res.Latest)
	}
	if res.HasUpdate {
		t.Fatal("HasUpdate = true; want false (server tag normalizes to current version)")
	}
}

func TestApply_ResolvesAssetNameUnderCliVTag(t *testing.T) {
	payload := []byte("fake-binary-bytes")
	tarball := makeTarballWithBinary(t, payload)
	hash := sha256.Sum256(tarball)
	checksums := fmt.Sprintf("%s  moltable_0.5.0_linux_amd64.tar.gz\n", hex.EncodeToString(hash[:]))

	srv := stubGitHubServer(t, "v0.5.0", tarball, checksums)
	defer srv.Close()

	c := &Client{
		BaseURL:  srv.URL,
		Repo:     "test/test",
		CacheDir: t.TempDir(),
	}
	err := c.Apply(context.Background(), "0.5.0", "linux", "amd64")
	if err != nil {
		if IsVerifyError(err) {
			t.Fatalf("Apply: unexpected verify error: %v", err)
		}
		if _, ok := err.(*AssetNotFoundError); ok {
			t.Fatalf("Apply: asset name lookup failed under v* tag: %v", err)
		}
		if !strings.Contains(err.Error(), "swap binary") {
			t.Fatalf("Apply: unexpected error: %v", err)
		}
	}
}

// canonicalReleaseTag regression — pin-by-version `moltable upgrade
// --version 0.5.0` must hit /releases/tags/v0.5.0 (the goreleaser
// monorepo tag), not /releases/tags/v0.5.0. A stub that records the
// actual URL the client requested would have caught the prior bug;
// the previous test's stub matched any /releases/tags/* path.
func TestFetchRelease_PinnedVersionUsesCliVTag(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{name: "bare semver", input: "0.5.0"},
		{name: "v-prefixed", input: "v0.5.0"},
		{name: "already canonical", input: "v0.5.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = r.URL.Path
				if strings.HasSuffix(r.URL.Path, "/releases/tags/v0.5.0") {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"tag_name":"v0.5.0","assets":[]}`))
					return
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			defer srv.Close()

			c := &Client{BaseURL: srv.URL, Repo: "test/test", CacheDir: t.TempDir()}
			c.applyDefaults()
			_, err := c.fetchRelease(context.Background(), tc.input)
			if err != nil {
				t.Fatalf("fetchRelease(%q) returned error: %v (server saw %q)", tc.input, err, seen)
			}
			if !strings.HasSuffix(seen, "/releases/tags/v0.5.0") {
				t.Fatalf("fetchRelease(%q) requested %q; want path ending in /releases/tags/v0.5.0", tc.input, seen)
			}
		})
	}
}

func TestCheckLatest_NetworkErrorWrapped(t *testing.T) {
	// Point at a closed server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	c := &Client{
		BaseURL:  srv.URL,
		Repo:     "test/test",
		CacheDir: t.TempDir(),
	}
	_, err := c.CheckLatest(context.Background(), "0.1.0")
	if err == nil {
		t.Fatal("expected error; got nil")
	}
	if !IsNetworkError(err) {
		t.Fatalf("err = %v; want *NetworkError", err)
	}
}

func TestApply_VerifiesChecksumAndExtracts(t *testing.T) {
	payload := []byte("fake-binary-bytes")
	tarball := makeTarballWithBinary(t, payload)
	hash := sha256.Sum256(tarball)
	checksums := fmt.Sprintf("%s  moltable_0.5.0_linux_amd64.tar.gz\n", hex.EncodeToString(hash[:]))

	srv := stubGitHubServer(t, "v0.5.0", tarball, checksums)
	defer srv.Close()

	c := &Client{
		BaseURL:  srv.URL,
		Repo:     "test/test",
		CacheDir: t.TempDir(),
	}

	// We can't actually swap the test binary, but we can drive the
	// pre-swap flow up to the point go-update tries. go-update's Apply
	// uses os.Args[0] to locate the running binary; on a successful
	// path it writes a sibling temp file and renames. For test
	// purposes we let it run — go-update writes to a tempfile alongside
	// the test binary which gets cleaned up by the test framework.
	//
	// The critical guarantee is that we got past download + verify +
	// extract without error, which is what failing-checksum tests
	// elsewhere assert the opposite of.
	err := c.Apply(context.Background(), "0.5.0", "linux", "amd64")
	if err != nil {
		// On some test environments go-update can't write next to the
		// running test binary; allow that specific stage to fail as
		// long as verification passed (no VerifyError).
		if IsVerifyError(err) {
			t.Fatalf("Apply: unexpected verify error: %v", err)
		}
		if _, ok := err.(*AssetNotFoundError); ok {
			t.Fatalf("Apply: unexpected asset-not-found: %v", err)
		}
		// Tolerate go-update's swap-stage failure since the test
		// binary is unwriteable. The error message from go-update
		// surfaces as "swap binary: ..." per our wrapper.
		if !strings.Contains(err.Error(), "swap binary") {
			t.Fatalf("Apply: unexpected error: %v", err)
		}
	}
}

func TestApply_ChecksumMismatchAbortsBeforeSwap(t *testing.T) {
	payload := []byte("fake-binary-bytes")
	tarball := makeTarballWithBinary(t, payload)
	// Intentionally wrong hash.
	checksums := "deadbeef  moltable_0.5.0_linux_amd64.tar.gz\n"

	srv := stubGitHubServer(t, "v0.5.0", tarball, checksums)
	defer srv.Close()

	// Record the running test binary's mtime as our "did we swap" tell.
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	before, err := os.Stat(self)
	if err != nil {
		t.Fatalf("stat self: %v", err)
	}

	c := &Client{
		BaseURL:  srv.URL,
		Repo:     "test/test",
		CacheDir: t.TempDir(),
	}
	err = c.Apply(context.Background(), "0.5.0", "linux", "amd64")
	if err == nil {
		t.Fatal("expected error; got nil")
	}
	if !IsVerifyError(err) {
		t.Fatalf("err = %v; want *VerifyError", err)
	}

	after, err := os.Stat(self)
	if err != nil {
		t.Fatalf("stat self: %v", err)
	}
	if before.ModTime() != after.ModTime() {
		t.Fatalf("test binary mtime changed: before=%v after=%v (should be untouched on verify failure)",
			before.ModTime(), after.ModTime())
	}
}

func TestApply_ReleaseNotFoundOn404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := &Client{
		BaseURL:  srv.URL,
		Repo:     "test/test",
		CacheDir: t.TempDir(),
	}
	err := c.Apply(context.Background(), "99.99.99", "linux", "amd64")
	if err == nil {
		t.Fatal("expected error; got nil")
	}
	var nfe *ReleaseNotFoundError
	if !asTypedErr(err, &nfe) {
		t.Fatalf("err = %v; want *ReleaseNotFoundError", err)
	}
}

func TestFindChecksum(t *testing.T) {
	content := `abc123  moltable_0.5.0_linux_amd64.tar.gz
def456  moltable_0.5.0_darwin_arm64.tar.gz
ghi789  *moltable_0.5.0_windows_amd64.zip
`
	h, ok := findChecksum([]byte(content), "moltable_0.5.0_darwin_arm64.tar.gz")
	if !ok {
		t.Fatal("expected found")
	}
	if h != "def456" {
		t.Fatalf("hash = %q; want def456", h)
	}

	// `*` prefix should not block matching (sha256sum -b style).
	h, ok = findChecksum([]byte(content), "moltable_0.5.0_windows_amd64.zip")
	if !ok {
		t.Fatal("expected found (with * prefix)")
	}
	if h != "ghi789" {
		t.Fatalf("hash = %q; want ghi789", h)
	}

	_, ok = findChecksum([]byte(content), "missing.tar.gz")
	if ok {
		t.Fatal("expected not-found")
	}
}

func TestExtractTarballBinary(t *testing.T) {
	payload := []byte("hello world binary")
	tarball := makeTarballWithBinary(t, payload)
	got, err := extractTarballBinary(tarball, "moltable")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("payload = %q; want %q", got, payload)
	}

	_, err = extractTarballBinary(tarball, "notpresent")
	if err == nil {
		t.Fatal("expected error for missing binary; got nil")
	}
}

// asTypedErr is a small helper so the test file doesn't take a
// standalone errors import just for errors.As.
func asTypedErr(err error, target interface{}) bool {
	type unwrapper interface{ Unwrap() error }
	for err != nil {
		switch t := target.(type) {
		case **ReleaseNotFoundError:
			if v, ok := err.(*ReleaseNotFoundError); ok {
				*t = v
				return true
			}
		case **OversizedDownloadError:
			if v, ok := err.(*OversizedDownloadError); ok {
				*t = v
				return true
			}
		}
		if u, ok := err.(unwrapper); ok {
			err = u.Unwrap()
			continue
		}
		return false
	}
	return false
}

// TestDownloadAsset_ContentLengthTooLarge verifies the pre-check: when
// the server advertises an oversized Content-Length, downloadAsset must
// return *OversizedDownloadError BEFORE any body bytes are read. We
// prove "before read" by attaching a body that blocks until the test
// signals release, asserting the call returns immediately, and only
// then unblocking the handler so httptest.Server.Close() can drain.
func TestDownloadAsset_ContentLengthTooLarge(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", int64(maxTarballSize+1)))
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		if flusher != nil {
			flusher.Flush()
		}
		// If the pre-check works the client closed the body already;
		// we just need to NOT return until the test says so, otherwise
		// httptest considers the connection done.
		<-release
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	c := &Client{
		BaseURL:    srv.URL,
		Repo:       "test/test",
		CacheDir:   t.TempDir(),
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
	c.applyDefaults()

	start := time.Now()
	_, err := c.downloadAsset(context.Background(), srv.URL+"/")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error; got nil")
	}
	var ode *OversizedDownloadError
	if !asTypedErr(err, &ode) {
		t.Fatalf("err = %v (%T); want *OversizedDownloadError", err, err)
	}
	if ode.Size != int64(maxTarballSize+1) {
		t.Fatalf("ode.Size = %d; want %d (advertised Content-Length)", ode.Size, int64(maxTarballSize+1))
	}
	if ode.MaxSize != int64(maxTarballSize) {
		t.Fatalf("ode.MaxSize = %d; want %d", ode.MaxSize, int64(maxTarballSize))
	}
	// Pre-check must fire before reading the body. 1s leaves plenty
	// of slack for CI machines — if it took longer, the limiter is
	// running after some body read instead of before.
	if elapsed >= 1*time.Second {
		t.Fatalf("downloadAsset took %v; expected <1s (Content-Length pre-check should fire before body read)", elapsed)
	}
}

// TestDownloadAsset_ChunkedOversize verifies the post-read limiter:
// when the server omits Content-Length and streams more than the cap
// via chunked transfer encoding, downloadAsset must still return
// *OversizedDownloadError instead of allocating the full payload.
func TestDownloadAsset_ChunkedOversize(t *testing.T) {
	// We don't actually need to write maxTarballSize+1 bytes — that
	// would mean allocating 200MB+ in the test. Instead, lower the
	// effective cap by writing a payload that's small in absolute terms
	// but exercises the same code path: write maxTarballSize+1024 zero
	// bytes via chunked encoding (httptest defaults to chunked when no
	// Content-Length is set).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Intentionally omit Content-Length to force chunked encoding.
		w.WriteHeader(http.StatusOK)
		// Write maxTarballSize+1024 bytes in 1MiB chunks to keep memory
		// pressure on the test process modest while still tripping the
		// LimitReader.
		chunk := make([]byte, 1<<20)
		written := int64(0)
		target := int64(maxTarballSize) + 1024
		flusher, _ := w.(http.Flusher)
		for written < target {
			n := int64(len(chunk))
			if written+n > target {
				n = target - written
			}
			if _, err := w.Write(chunk[:n]); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			written += n
		}
	}))
	defer srv.Close()

	c := &Client{
		BaseURL:    srv.URL,
		Repo:       "test/test",
		CacheDir:   t.TempDir(),
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
	c.applyDefaults()

	_, err := c.downloadAsset(context.Background(), srv.URL+"/")
	if err == nil {
		t.Fatal("expected error; got nil")
	}
	var ode *OversizedDownloadError
	if !asTypedErr(err, &ode) {
		t.Fatalf("err = %v (%T); want *OversizedDownloadError", err, err)
	}
	if ode.Size != -1 {
		t.Fatalf("ode.Size = %d; want -1 (streaming limiter caught oversize, no advertised length)", ode.Size)
	}
	if ode.MaxSize != int64(maxTarballSize) {
		t.Fatalf("ode.MaxSize = %d; want %d", ode.MaxSize, int64(maxTarballSize))
	}
}

// TestDownloadAsset_UnderCap_StillWorks regression-pins the happy path:
// a small valid payload returns its bytes unchanged. Catches the case
// where we accidentally cap at the wrong value or eat bytes via the
// LimitReader wrapper.
func TestDownloadAsset_UnderCap_StillWorks(t *testing.T) {
	payload := []byte("hello-world-tarball-bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	c := &Client{
		BaseURL:    srv.URL,
		Repo:       "test/test",
		CacheDir:   t.TempDir(),
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
	c.applyDefaults()

	got, err := c.downloadAsset(context.Background(), srv.URL+"/")
	if err != nil {
		t.Fatalf("downloadAsset: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload = %q; want %q", got, payload)
	}
}
