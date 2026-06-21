// Package version holds the compile-time version constants the CLI
// uses for its own self-identification, its server-floor check, and
// the User-Agent header it stamps on every outbound request.
//
// `BinaryVersion` is overridden at release time by goreleaser via
// `-ldflags "-X .../internal/version.BinaryVersion=v0.5.0"`. Local
// dev builds keep the literal default below, which lets `moltable
// version` print something meaningful without ldflags noise.
//
// `MinServerVersion` is the OLDEST X-Moltable-Version the CLI will
// accept on the wire. When the server's advertised version (in the
// `X-Moltable-Version` response header) is older than this floor,
// the HTTP layer raises errors.ServerTooOldError and the caller
// exits non-zero with a "upgrade the server or downgrade the CLI"
// hint. The server-side middleware that stamps that response header
// is what makes the check reliable.
//
// Both constants live in their own package — NOT in `main` — so any
// internal package (updater, httpc, output) can read them without
// taking a circular dependency on the entry point.
package version

// BinaryVersion is the version string this binary advertises in its
// User-Agent header and in `moltable version` output. Overridden at
// build time by goreleaser ldflags.
var BinaryVersion = "0.1.0-dev"

// BinaryCommit is the git commit short SHA the binary was built from.
// Empty in local `go build` / `go install` invocations. Injected by
// goreleaser ldflags `-X .../internal/version.BinaryCommit={{.ShortCommit}}`
// for tagged releases.
//
// Surfaced in `moltable version --json` as `commit` so a user filing a
// bug report can paste the exact build that's misbehaving, matching the
// X-Moltable-Version pattern the API server already follows.
var BinaryCommit = ""

// BinaryBuildDate is the ISO-8601 UTC timestamp the binary was built
// at, injected by goreleaser ldflags `-X .../internal/version.BinaryBuildDate={{.Date}}`.
// Empty in local builds; surfaced as `build_date` in `moltable version --json`.
var BinaryBuildDate = ""

// MinServerVersion is the lower bound on the X-Moltable-Version response
// header the CLI will tolerate. If the server is older, the HTTP layer
// surfaces *errors.ServerTooOldError to the caller.
const MinServerVersion = "0.1.0"
