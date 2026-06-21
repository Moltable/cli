// Package updater performs in-place self-update of the moltable binary
// against the canonical GitHub releases for `moltable/cli`.
//
// Two entry points:
//
//   - CheckLatest(ctx, currentVersion) — used by both `moltable upgrade
//     --check-only` and the background nudge goroutine. Reads a 24-hour
//     cache at ~/.config/moltable/update-check.json before falling back
//     to the GitHub releases API, so a `moltable` invocation never pays
//     the round-trip latency on the hot path.
//   - Apply(ctx, version, currentOS, currentArch) — full download +
//     sha256 verify + atomic swap. Pass empty string for the latest
//     release; explicit pinning (e.g. "0.5.0") is honored via the
//     `?version=` GitHub redirect.
//
// Why we don't use go-update's HTTP fetch helpers directly: we want
// strict sha256 verification BEFORE the swap, against a `checksums.txt`
// shipped alongside the tarball. go-update's Apply() accepts an
// `io.Reader` of plain binary bytes, so we do the fetch + verify +
// extract ourselves and only hand it the verified binary stream.
//
// On checksum mismatch we abort and the on-disk binary is unchanged —
// asserted by the corresponding test in updater_test.go.
//
// After a successful Apply, the upgrade command re-invokes
// `moltable skills install` so the embedded skill bundle on disk matches
// the just-installed binary. That stays in upgrade.go so this package
// has no dependency on os/exec or the CLI's other commands.
package updater

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	goupdate "github.com/inconshreveable/go-update"

	"archive/tar"
	"compress/gzip"
)

// DefaultReleaseRepo is the canonical GitHub repo that hosts releases.
// Tests override via NewClient(repo: "owner/repo", ...).
const DefaultReleaseRepo = "moltable/cli"

// CacheFileName is the on-disk update-check cache filename. Lives next
// to the user's TOML config (~/.config/moltable/).
const CacheFileName = "update-check.json"

// CacheTTL is the maximum age of a cached check before CheckLatest will
// re-fetch from GitHub.
const CacheTTL = 24 * time.Hour

// maxTarballSize bounds how many bytes downloadAsset will pull off the
// wire before aborting with *OversizedDownloadError. The 30s http.Client
// timeout bounds wall time but not throughput, so without an explicit
// byte cap a fast pipe could stream gigabytes into RAM. Real moltable
// release tarballs are <20MB; 200MB leaves 10x headroom for future
// growth while still aborting long before the host OOMs.
const maxTarballSize = 200 << 20

// CacheEntry is the JSON shape persisted at ~/.config/moltable/update-check.json.
// Format is intentionally tiny — just the latest known version + the
// time we observed it. Forward compatibility: unknown fields are
// preserved on read by the JSON decoder so an older binary won't wipe
// fields written by a newer one.
type CacheEntry struct {
	LatestVersion string    `json:"latest_version"`
	CheckedAt     time.Time `json:"checked_at"`
}

// Result is what CheckLatest returns. HasUpdate is the result of a
// dumb string-inequality compare — semver math (e.g. "is 0.5.0 newer
// than 0.4.9") lives in the caller (cmd/moltable/upgrade.go), keeping
// this package free of semver parsing.
type Result struct {
	Latest    string
	HasUpdate bool
}

// Client is the configurable update client. Tests construct one with a
// custom BaseURL pointing at an httptest.Server.
type Client struct {
	// BaseURL is the GitHub API host. Defaults to https://api.github.com.
	BaseURL string
	// DownloadHost is the host serving the release assets (tarball,
	// checksums). In production GitHub redirects from api.github.com to
	// objects.githubusercontent.com; tests can pin a single host.
	DownloadHost string
	// Repo is owner/repo. Defaults to DefaultReleaseRepo.
	Repo string
	// HTTPClient is the underlying transport. Defaults to a 30s-timeout
	// http.Client so a hung GitHub doesn't block a CLI invocation
	// indefinitely.
	HTTPClient *http.Client
	// CacheDir is where the 24h update-check cache lives. Defaults to
	// the moltable config dir.
	CacheDir string
	// Now is the clock — overridable in tests so cache-TTL logic is
	// deterministic.
	Now func() time.Time
}

// NewClient returns a Client populated with production defaults. Tests
// typically construct one inline rather than calling this.
func NewClient() *Client {
	return &Client{
		BaseURL:    "https://api.github.com",
		Repo:       DefaultReleaseRepo,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		Now:        time.Now,
	}
}

// ghReleaseAsset mirrors the subset of github.com/api/v3 release-asset
// JSON we care about. Other fields are ignored on decode.
type ghReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// ghReleaseResponse is the trimmed shape of GET /repos/{o}/{r}/releases/latest
// (and /releases/tags/{tag} for pinned versions).
type ghReleaseResponse struct {
	TagName string           `json:"tag_name"`
	Assets  []ghReleaseAsset `json:"assets"`
}

// CheckLatest returns the latest version known to GitHub (subject to
// the 24h cache) and a HasUpdate flag computed by string inequality
// against currentVersion. Network errors leave a stale cache untouched.
//
// The cache prevents `moltable <anything>` from paying GitHub's
// round-trip on the hot path: every CLI invocation can spawn a
// fire-and-forget CheckLatest, and 99% of the time it returns from
// disk in microseconds.
func (c *Client) CheckLatest(ctx context.Context, currentVersion string) (Result, error) {
	c.applyDefaults()

	// Read cache first. If fresh, short-circuit — no network.
	if entry, ok := c.readCache(); ok && c.cacheFresh(entry) {
		return Result{
			Latest:    entry.LatestVersion,
			HasUpdate: entry.LatestVersion != "" && entry.LatestVersion != currentVersion,
		}, nil
	}

	rel, err := c.fetchRelease(ctx, "")
	if err != nil {
		return Result{}, err
	}
	latest := stripVersionPrefix(rel.TagName)

	// Write cache best-effort. A failure to persist the cache should
	// never break a successful check — we already have the answer in
	// memory.
	_ = c.writeCache(CacheEntry{LatestVersion: latest, CheckedAt: c.Now()})

	return Result{
		Latest:    latest,
		HasUpdate: latest != "" && latest != currentVersion,
	}, nil
}

// Apply downloads the requested version (or latest, when version is
// empty), verifies its sha256 against checksums.txt, extracts the
// `moltable` binary from the tarball, and atomically replaces the
// running executable via go-update.
//
// Verification ordering is strict: we ONLY swap after the hash matches.
// A mismatch returns a typed *VerifyError and the running binary on
// disk is unchanged.
func (c *Client) Apply(ctx context.Context, version, runtimeOS, runtimeArch string) error {
	c.applyDefaults()

	rel, err := c.fetchRelease(ctx, version)
	if err != nil {
		return err
	}

	resolvedTag := stripVersionPrefix(rel.TagName)
	tarballName := fmt.Sprintf("moltable_%s_%s_%s.tar.gz", resolvedTag, runtimeOS, runtimeArch)

	var tarballURL, checksumsURL string
	for _, a := range rel.Assets {
		switch a.Name {
		case tarballName:
			tarballURL = a.BrowserDownloadURL
		case "checksums.txt":
			checksumsURL = a.BrowserDownloadURL
		}
	}
	if tarballURL == "" {
		return &AssetNotFoundError{Name: tarballName, Tag: rel.TagName}
	}
	if checksumsURL == "" {
		return &AssetNotFoundError{Name: "checksums.txt", Tag: rel.TagName}
	}

	tarballBytes, err := c.downloadAsset(ctx, tarballURL)
	if err != nil {
		return fmt.Errorf("download %s: %w", tarballName, err)
	}
	checksumsBytes, err := c.downloadAsset(ctx, checksumsURL)
	if err != nil {
		return fmt.Errorf("download checksums.txt: %w", err)
	}

	wantHash, ok := findChecksum(checksumsBytes, tarballName)
	if !ok {
		return &VerifyError{Asset: tarballName, Reason: "no entry in checksums.txt"}
	}
	gotHashRaw := sha256.Sum256(tarballBytes)
	gotHash := hex.EncodeToString(gotHashRaw[:])
	if gotHash != wantHash {
		return &VerifyError{
			Asset:  tarballName,
			Reason: fmt.Sprintf("sha256 mismatch: got %s, want %s", gotHash, wantHash),
		}
	}

	binBytes, err := extractTarballBinary(tarballBytes, "moltable")
	if err != nil {
		return fmt.Errorf("extract moltable from %s: %w", tarballName, err)
	}

	if err := goupdate.Apply(bytes.NewReader(binBytes), goupdate.Options{}); err != nil {
		return fmt.Errorf("swap binary: %w", err)
	}
	return nil
}

// canonicalReleaseTag turns whatever shape the caller supplies for a
// pinned version ("0.5.0" / "v0.5.0" / "v0.5.0") into the exact
// tag goreleaser publishes ("v0.5.0"). Inverse of
// stripVersionPrefix.
func canonicalReleaseTag(version string) string {
	bare := stripVersionPrefix(version)
	return "v" + bare
}

// stripVersionPrefix normalizes a GH release tag down to its bare semver.
// The repo tags CLI releases as `vX.Y.Z`; the currentVersion baked into
// the binary via ldflags is the bare `X.Y.Z`. The legacy `cli-` strip
// stays in case anyone has tags from before the cli-v→v migration
// (no-op on modern tags). Cheap defense, no failure mode.
func stripVersionPrefix(tag string) string {
	return strings.TrimPrefix(strings.TrimPrefix(tag, "cli-"), "v")
}

// applyDefaults fills nil/zero fields with production defaults so
// callers don't have to set them. Tests that DO set them keep their
// custom values because we only touch zero-valued fields.
func (c *Client) applyDefaults() {
	if c.BaseURL == "" {
		c.BaseURL = "https://api.github.com"
	}
	if c.Repo == "" {
		c.Repo = DefaultReleaseRepo
	}
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// fetchRelease GETs /repos/{o}/{r}/releases/latest (when version is
// empty) or /releases/tags/v{version} (when pinned).
//
// The "v" prefix mirrors the monorepo tag prefix configured in
// .goreleaser.yaml — goreleaser publishes releases as `vX.Y.Z`
// so the GitHub release exists at /releases/tags/vX.Y.Z. Building
// a bare `v0.5.0` here would 404 against every real release.
//
// We accept the user-supplied version in three shapes and canonicalize:
//   - "0.5.0"        -> "v0.5.0"
//   - "v0.5.0"       -> "v0.5.0"
//   - "v0.5.0"   -> "v0.5.0" (no double-prefix)
func (c *Client) fetchRelease(ctx context.Context, version string) (*ghReleaseResponse, error) {
	var endpoint string
	if version == "" {
		endpoint = fmt.Sprintf("%s/repos/%s/releases/latest", c.BaseURL, c.Repo)
	} else {
		tag := canonicalReleaseTag(version)
		endpoint = fmt.Sprintf("%s/repos/%s/releases/tags/%s", c.BaseURL, c.Repo, url.PathEscape(tag))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, &NetworkError{Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, &ReleaseNotFoundError{Tag: version}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &NetworkError{Err: fmt.Errorf("github responded %d", resp.StatusCode)}
	}

	var rel ghReleaseResponse
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("parse github response: %w", err)
	}
	return &rel, nil
}

// downloadAsset GETs the given URL and returns the body bytes. Bounded
// by the Client's http timeout AND by maxTarballSize so a fast source
// can't OOM the CLI by streaming gigabytes before the timer fires.
//
// Two-stage size check:
//
//   - If the server advertises a Content-Length larger than the cap, we
//     bail before reading a single body byte.
//   - For chunked / unknown-length responses we wrap the body in an
//     io.LimitReader at cap+1 and reject anything that hits the extra
//     byte. Both paths return the same *OversizedDownloadError so callers
//     don't have to distinguish.
func (c *Client) downloadAsset(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, &NetworkError{Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &NetworkError{Err: fmt.Errorf("download %s: status %d", rawURL, resp.StatusCode)}
	}
	if resp.ContentLength > maxTarballSize {
		return nil, &OversizedDownloadError{
			URL:     rawURL,
			Size:    resp.ContentLength,
			MaxSize: maxTarballSize,
		}
	}
	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxTarballSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) > maxTarballSize {
		return nil, &OversizedDownloadError{
			URL:     rawURL,
			Size:    -1,
			MaxSize: maxTarballSize,
		}
	}
	return buf, nil
}

// cacheFresh reports whether the cache entry is younger than CacheTTL.
func (c *Client) cacheFresh(e CacheEntry) bool {
	if e.CheckedAt.IsZero() {
		return false
	}
	return c.Now().Sub(e.CheckedAt) < CacheTTL
}

// cachePath resolves the on-disk cache location, honoring Client.CacheDir
// for tests.
func (c *Client) cachePath() (string, error) {
	if c.CacheDir != "" {
		return filepath.Join(c.CacheDir, CacheFileName), nil
	}
	dir, err := defaultCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, CacheFileName), nil
}

func (c *Client) readCache() (CacheEntry, bool) {
	path, err := c.cachePath()
	if err != nil {
		return CacheEntry{}, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return CacheEntry{}, false
	}
	var e CacheEntry
	if err := json.Unmarshal(raw, &e); err != nil {
		return CacheEntry{}, false
	}
	return e, true
}

func (c *Client) writeCache(e CacheEntry) error {
	path, err := c.cachePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	// Best-effort write; not atomic — racing the cache file between two
	// concurrent CLI invocations is harmless (last writer wins, both
	// values are equally correct).
	return os.WriteFile(path, raw, 0o600)
}

// defaultCacheDir resolves the moltable config dir (same place as
// the TOML config). Honors XDG_CONFIG_HOME, then $HOME/.config.
func defaultCacheDir() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "moltable"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "moltable"), nil
}

// findChecksum scans a `sha256sum`-style checksums.txt for the line
// matching `name` and returns the hex hash. Lines look like:
//
//	abc123…def  moltable_0.5.0_linux_amd64.tar.gz
//
// Returns ("", false) if the asset isn't listed.
func findChecksum(data []byte, name string) (string, bool) {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// sha256sum -b prefixes the filename with `*`; strip it.
		fname := strings.TrimPrefix(fields[len(fields)-1], "*")
		if fname == name {
			return fields[0], true
		}
	}
	return "", false
}

// extractTarballBinary opens a tar.gz blob and returns the bytes of
// the entry whose base name matches `binaryName`. Used to pull the
// `moltable` executable out of the goreleaser tarball without writing
// any temp files.
func extractTarballBinary(tarball []byte, binaryName string) ([]byte, error) {
	gz, err := gzip.NewReader(strings.NewReader(string(tarball)))
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar: %w", err)
		}
		if filepath.Base(hdr.Name) != binaryName {
			continue
		}
		// Bound the read to a few hundred MB so a malformed tar header
		// claiming gigabytes can't OOM the process.
		const maxBinarySize = 500 * 1024 * 1024
		buf, err := io.ReadAll(io.LimitReader(tr, maxBinarySize+1))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", binaryName, err)
		}
		if int64(len(buf)) > maxBinarySize {
			return nil, fmt.Errorf("%s exceeds %d byte limit", binaryName, maxBinarySize)
		}
		return buf, nil
	}
	return nil, fmt.Errorf("%s not found in tarball", binaryName)
}

// --- Typed errors -------------------------------------------------

// NetworkError signals a transport-level failure reaching GitHub.
type NetworkError struct{ Err error }

func (e *NetworkError) Error() string {
	return fmt.Sprintf("updater: network: %v", e.Err)
}
func (e *NetworkError) Unwrap() error { return e.Err }
func (e *NetworkError) UserMessage() string {
	return "Could not fetch latest release. Check your internet connection."
}
func (e *NetworkError) Hint() string {
	return "Retry once network is available; meanwhile the existing binary keeps working."
}

// OversizedDownloadError signals the release asset exceeded the
// hard-coded maxTarballSize cap, either by advertised Content-Length or
// by streaming past the limit on a chunked response. We refuse to
// allocate gigabytes off the wire — a real release tarball is <20MB, so
// an oversized payload most likely indicates an upstream artifact swap
// or a misconfigured CDN.
type OversizedDownloadError struct {
	URL     string
	Size    int64 // advertised Content-Length, or -1 when only the streaming limiter caught it
	MaxSize int64
}

func (e *OversizedDownloadError) Error() string {
	if e.Size > 0 {
		return fmt.Sprintf("updater: download %s: %d bytes exceeds %d byte cap", e.URL, e.Size, e.MaxSize)
	}
	return fmt.Sprintf("updater: download %s: response exceeds %d byte cap", e.URL, e.MaxSize)
}
func (e *OversizedDownloadError) UserMessage() string {
	return "Release artifact is unexpectedly large; refusing to download. This may indicate tampering with the published release."
}
func (e *OversizedDownloadError) Hint() string {
	return "File an issue at https://github.com/moltable/cli/issues with the version you tried to install."
}

// ReleaseNotFoundError signals GitHub returned 404 for the requested
// release tag. Most commonly: the user typed `--version 9.9.9` and no
// such tag exists.
type ReleaseNotFoundError struct{ Tag string }

func (e *ReleaseNotFoundError) Error() string {
	if e.Tag == "" {
		return "updater: no releases published"
	}
	return fmt.Sprintf("updater: release %q not found", e.Tag)
}
func (e *ReleaseNotFoundError) UserMessage() string {
	if e.Tag == "" {
		return "No moltable releases are currently published."
	}
	return fmt.Sprintf("Release %q not found.", e.Tag)
}
func (e *ReleaseNotFoundError) Hint() string {
	return "Run `moltable upgrade --check-only` to see the latest available version."
}

// AssetNotFoundError signals the release exists but doesn't contain a
// matching asset for the current platform.
type AssetNotFoundError struct {
	Name string
	Tag  string
}

func (e *AssetNotFoundError) Error() string {
	return fmt.Sprintf("updater: asset %q missing from release %s", e.Name, e.Tag)
}
func (e *AssetNotFoundError) UserMessage() string {
	return fmt.Sprintf("Release %s has no asset for this platform (%s).", e.Tag, e.Name)
}
func (e *AssetNotFoundError) Hint() string {
	return "Try `moltable upgrade --version <other>`, or report the missing asset."
}

// VerifyError signals a sha256 mismatch — the downloaded binary did
// NOT match its declared hash and we refused to swap.
type VerifyError struct {
	Asset  string
	Reason string
}

func (e *VerifyError) Error() string {
	return fmt.Sprintf("updater: verify %s: %s", e.Asset, e.Reason)
}
func (e *VerifyError) UserMessage() string {
	return "Downloaded binary failed checksum verification. Aborting."
}
func (e *VerifyError) Hint() string {
	return "Retry the upgrade; persistent failures may indicate a CDN or release-process issue."
}

// IsNetworkError reports whether err is or wraps a NetworkError.
func IsNetworkError(err error) bool {
	var ne *NetworkError
	return errors.As(err, &ne)
}

// IsVerifyError reports whether err is or wraps a VerifyError.
func IsVerifyError(err error) bool {
	var ve *VerifyError
	return errors.As(err, &ve)
}
