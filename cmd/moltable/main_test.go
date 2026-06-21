package main

import (
	"strings"
	"testing"
)

// usingNounAuth must correctly distinguish "user typed bare `moltable
// auth`" from "user supplied `auth` as the VALUE of --profile" — the
// latter would otherwise trigger the smart-default and incorrectly
// rewrite the args to `[--profile, auth, login]`, which Kong rejects
// after the CLI already printed a misleading `-> running auth login`
// status line to stderr.
func TestUsingNounAuth(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		// Genuine bare-`auth` shapes that SHOULD trigger smart-default.
		{name: "bare auth", args: []string{"auth"}, want: true},
		{name: "global --dev then auth", args: []string{"--dev", "auth"}, want: true},
		{name: "global --json-style flag then auth", args: []string{"--profile=prod", "auth"}, want: true},
		{name: "auth then global flag", args: []string{"auth", "--dev"}, want: true},
		{name: "auth between flags", args: []string{"--dev", "auth", "--profile=prod"}, want: true},

		// `auth` consumed as the VALUE of a space-separated value-taking flag.
		// These must NOT trigger smart-default — the user's actual command
		// shape is `--profile auth <noun> <verb>` (profile named auth).
		{name: "--profile auth + workbook list", args: []string{"--profile", "auth", "workbook", "list"}, want: false},
		{name: "--api-key auth + workbook list", args: []string{"--api-key", "auth", "workbook", "list"}, want: false},
		{name: "--config auth + workbook list", args: []string{"--config", "auth", "workbook", "list"}, want: false},

		// Anything beyond bare `auth` is not a smart-default candidate.
		{name: "auth login (real verb)", args: []string{"auth", "login"}, want: false},
		{name: "auth then noun", args: []string{"auth", "workbook"}, want: false},
		{name: "bare workbook", args: []string{"workbook"}, want: false},
		{name: "empty args", args: []string{}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := usingNounAuth(tc.args); got != tc.want {
				t.Errorf("usingNounAuth(%v) = %v; want %v", tc.args, got, tc.want)
			}
		})
	}
}

// Smart-default refuses to launch the browser-handoff dance when
// stderr isn't a TTY. runCLI's tempfile-backed streams are NOT TTYs,
// so the guard always fires under the test harness. Without the
// guard, a CI script that ran `moltable auth` casually would hang
// the poll loop for 5 minutes and pop a browser tab nobody can see.
func TestRunAuthDefault_RefusesLoginInNonTTY(t *testing.T) {
	t.Setenv("MOLTABLE_NO_UPDATE_CHECK", "1")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("MOLTABLE_API_KEY", "")
	t.Setenv("MOLTABLE_PROFILE", "")

	code, stdout, stderr := runCLI(t, "auth")
	if code != 2 {
		t.Fatalf("exit = %d; want 2 (auth error)\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	se := stderr.String()
	if !strings.Contains(se, "not a terminal") {
		t.Errorf("stderr missing TTY-guard sentence; got %q", se)
	}
	if !strings.Contains(se, "hint:") {
		t.Errorf("stderr missing hint line; got %q", se)
	}
	// Negative check: smart-default must NOT have run the dance.
	if strings.Contains(se, "Open this URL") || strings.Contains(se, "verification_uri") {
		t.Errorf("smart-default leaked the browser-handoff URL despite non-TTY stderr; got %q", se)
	}
}

// printIntroAfterMissingCommand re-parses with `--help` appended.
// Kong's default behavior is to call os.Exit(0) when it sees --help,
// which would terminate the in-process test harness. The fix builds
// a throwaway parser with kong.Exit overridden — this test exists
// because if that override regresses, the test runner itself would
// die mid-test and we'd see no failure (just a missing test). Asserting
// on the exit code + the help banner confirms run() returned cleanly.
func TestBareCommand_HelpDoesNotOsExit(t *testing.T) {
	t.Setenv("MOLTABLE_NO_UPDATE_CHECK", "1")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("MOLTABLE_API_KEY", "")
	t.Setenv("MOLTABLE_PROFILE", "")

	code, stdout, _ := runCLI(t, "workbook") // bare noun → scoped help, no smart default
	if code != 0 {
		t.Fatalf("exit = %d; want 0 (bare noun is exploration, not error)", code)
	}
	so := stdout.String()
	if !strings.Contains(so, "Usage:") && !strings.Contains(so, "workbook") {
		t.Errorf("bare-noun stdout missing scoped help; got %q", so)
	}
}
