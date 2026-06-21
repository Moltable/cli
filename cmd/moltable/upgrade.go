// `moltable upgrade` — fetch the latest (or pinned) release from
// GitHub, verify its sha256, swap the running binary, then re-run
// `moltable skills install` so the on-disk skill bundle matches the
// just-installed binary.
//
// Flags:
//
//   - --version <X>   pin to a specific release tag (e.g. "0.5.0").
//                     Empty → fetch latest.
//   - --check-only    do not download. Just print latest vs current
//                     and exit 0 if newer exists, 1 if current is
//                     latest. Used by the background nudge.
//
// Errors are rendered through the existing typed-error chain in
// main.go's run(): the updater package returns *NetworkError,
// *VerifyError, *ReleaseNotFoundError, *AssetNotFoundError — each
// satisfies UserMessage() and Hint() so the user sees a single
// complete sentence + a next-action hint on stderr.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/alecthomas/kong"

	"github.com/moltable/cli/internal/output"
	"github.com/moltable/cli/internal/updater"
	clversion "github.com/moltable/cli/internal/version"
)

// runUpgrade executes the full upgrade flow OR the check-only flow
// depending on the flags. Kept out of the command struct so tests
// can drive it directly and inject a stub updater.Client.
func runUpgrade(kctx *kong.Context, version string, checkOnly, jsonOut bool, jqExpr string) error {
	client := updater.NewClient()
	// Surface lifecycle milestones to stderr so `moltable upgrade`
	// doesn't look hung for the ~5-15s download + verify takes.
	// JSON-mode + check-only paths stay silent (no Apply call, or
	// machine-readable output that shouldn't be mixed with prose).
	if !checkOnly && !jsonOut {
		client.Progress = kctx.Stderr
	}
	return runUpgradeWithClient(kctx, client, version, checkOnly, jsonOut, jqExpr)
}

// runUpgradeWithClient is the testable form. The real CLI uses a
// production updater.NewClient(); tests can pass one with BaseURL
// pointing at an httptest.Server.
func runUpgradeWithClient(kctx *kong.Context, client *updater.Client, version string, checkOnly, jsonOut bool, jqExpr string) error {
	ctx := context.Background()

	if checkOnly {
		res, err := client.CheckLatest(ctx, clversion.BinaryVersion)
		if err != nil {
			return err
		}
		if jsonOut {
			return output.Print(kctx.Stdout, map[string]any{
				"current":    clversion.BinaryVersion,
				"latest":     res.Latest,
				"has_update": res.HasUpdate,
			}, jqExpr)
		}
		if res.HasUpdate {
			fmt.Fprintf(kctx.Stdout, "Update available: %s -> %s\n", clversion.BinaryVersion, res.Latest)
			return nil
		}
		fmt.Fprintf(kctx.Stdout, "moltable is up to date (%s).\n", clversion.BinaryVersion)
		// Return exit 1 when already on the latest — the background
		// update-check goroutine treats nonzero as "no nudge".
		// errCurrent is a silent sentinel that main.go maps to exit 1
		// with no extra stderr noise.
		return errCurrent
	}

	if err := client.Apply(ctx, version, runtime.GOOS, runtime.GOARCH); err != nil {
		return err
	}

	// After a successful swap, re-run `moltable skills install` so
	// the on-disk bundle matches the just-installed binary. Use the
	// path that go-update wrote to (os.Args[0] is the running process
	// — which IS the freshly swapped binary after Apply returns on
	// platforms where the rename is observable in-place).
	self, err := os.Executable()
	if err != nil {
		// We swapped the binary OK; just couldn't locate ourselves to
		// re-run skills install. Print a manual-step hint and call it
		// a successful upgrade.
		fmt.Fprintln(kctx.Stderr, "Upgraded; re-run `moltable skills install` to refresh skills.")
		return nil
	}
	cmd := exec.Command(self, "skills", "install")
	cmd.Stdout = kctx.Stdout
	cmd.Stderr = kctx.Stderr
	if err := cmd.Run(); err != nil {
		// Skills install failure post-upgrade is non-fatal — the
		// binary IS updated; the skills bundle just lags. Surface a
		// hint so the user knows the manual recovery step.
		fmt.Fprintln(kctx.Stderr, "Upgraded; `moltable skills install` failed — re-run it manually.")
		return nil
	}

	// Success line goes to stderr — stdout stays silent so scripts
	// piping `moltable upgrade` get a clean exit-0 stream.
	resolved := version
	if resolved == "" {
		resolved = "latest"
	}
	fmt.Fprintf(kctx.Stderr, "Upgraded to %s.\n", resolved)
	return nil
}

// errCurrent is the sentinel returned by `upgrade --check-only` when
// the current binary is already the latest. main.go's run() recognizes
// it and exits 1 without printing the "moltable: ..." prefix.
var errCurrent = errors.New("moltable: already at latest version")

// IsAlreadyCurrent reports whether err is errCurrent. Lets main.go
// and the background goroutine skip stderr noise for this expected case.
func IsAlreadyCurrent(err error) bool {
	return errors.Is(err, errCurrent)
}
