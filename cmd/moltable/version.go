// `moltable version` — print the CLI's compiled-in version, the git
// commit it was built from, the build date, and the minimum API
// version it tolerates on the wire.
//
// Two output shapes:
//
//   - human: `moltable 0.1.0 (commit abc1234, built 2026-06-19T12:00:00Z; min API version: 0.1.0)`
//   - JSON:  `{"binary_version":"0.1.0","commit":"abc1234","build_date":"2026-06-19T12:00:00Z","min_server_version":"0.1.0"}`
//
// `BinaryVersion`, `BinaryCommit`, `BinaryBuildDate` are overridden at
// release time by goreleaser via ldflags; in `go build`/`go install`
// dev invocations commit + build_date stay empty and are omitted from
// the human output (and emitted as empty strings in JSON so the schema
// is stable for agent parsers).
package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/alecthomas/kong"

	"github.com/moltable/cli/internal/output"
	clversion "github.com/moltable/cli/internal/version"
)

// versionPayload mirrors the X-Moltable-Version + commit-sha pattern
// the API already follows (see apps/api/cmd/server/main.go:40 and its
// Dockerfile injection of RAILWAY_GIT_COMMIT_SHA). Including commit +
// build_date in support output lets a bug report identify the exact
// build without needing the user to re-run anything.
type versionPayload struct {
	BinaryVersion    string `json:"binary_version"`
	Commit           string `json:"commit"`
	BuildDate        string `json:"build_date"`
	MinServerVersion string `json:"min_server_version"`
}

// buildUserAgent stamps every outbound request so backend access logs
// can identify the exact CLI build. Shape:
//
//	moltable-cli/<semver>+<commit>+dev (<hostname>; <os>/<arch>)
//
// The `+commit` segment is only present when ldflags injected one;
// `+dev` is only present under --dev / MOLTABLE_DEV. The
// `(<hostname>; <os>/<arch>)` suffix follows the conventional Mozilla
// UA shape so log scrapers can fingerprint devices; hostname is
// omitted when MOLTABLE_NO_HOSTNAME is set (privacy opt-out).
//
// Single source of truth so a header rename doesn't require a
// multi-file sweep.
func buildUserAgent(dev bool) string {
	ua := "moltable-cli/" + clversion.BinaryVersion
	if clversion.BinaryCommit != "" {
		ua += "+" + clversion.BinaryCommit
	}
	if dev {
		ua += "+dev"
	}
	// Device suffix. Hostname only if not opted out; OS/arch always
	// safe to ship (no PII).
	hostname := ""
	if os.Getenv(envNoHostname) == "" {
		hostname = sanitizeHostname(hostnameOrEmpty())
	}
	if hostname != "" {
		ua += fmt.Sprintf(" (%s; %s/%s)", hostname, runtime.GOOS, runtime.GOARCH)
	} else {
		ua += fmt.Sprintf(" (%s/%s)", runtime.GOOS, runtime.GOARCH)
	}
	return ua
}

// runVersion is called from the VersionCmd.Run override. Keeping the
// body out-of-struct keeps the struct-literal layout in main.go small
// and lets tests call runVersion directly without simulating kong.
func runVersion(kctx *kong.Context, jsonOut bool, jqExpr string) error {
	payload := versionPayload{
		BinaryVersion:    clversion.BinaryVersion,
		Commit:           clversion.BinaryCommit,
		BuildDate:        clversion.BinaryBuildDate,
		MinServerVersion: clversion.MinServerVersion,
	}
	if jsonOut {
		return output.Print(kctx.Stdout, payload, jqExpr)
	}
	// Human shape: only render the bits we actually have. A `go install`
	// dev build with no ldflags should NOT show "commit , built " — the
	// empty values look broken.
	parts := []string{}
	if payload.Commit != "" {
		parts = append(parts, "commit "+payload.Commit)
	}
	if payload.BuildDate != "" {
		parts = append(parts, "built "+payload.BuildDate)
	}
	parts = append(parts, "min API version: "+payload.MinServerVersion)
	_, err := fmt.Fprintf(
		kctx.Stdout,
		"moltable %s (%s)\n",
		payload.BinaryVersion, strings.Join(parts, ", "),
	)
	return err
}
