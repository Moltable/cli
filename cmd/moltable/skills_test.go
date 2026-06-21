package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	cli "github.com/moltable/cli"
)

// runCLI exercises the binary's run() function in-process so we can
// assert on stdout/stderr without spawning a subprocess. Returns the
// exit code along with captured streams.
func runCLI(t *testing.T, args ...string) (code int, stdout, stderr *bytes.Buffer) {
	t.Helper()
	// We can't easily synthesize *os.File-backed buffers, and run()
	// expects *os.File. Use a temp file pair as a workaround.
	stdoutF, err := os.CreateTemp(t.TempDir(), "stdout-*.log")
	if err != nil {
		t.Fatalf("create stdout temp: %v", err)
	}
	defer stdoutF.Close()
	stderrF, err := os.CreateTemp(t.TempDir(), "stderr-*.log")
	if err != nil {
		t.Fatalf("create stderr temp: %v", err)
	}
	defer stderrF.Close()

	code = run(args, stdoutF, stderrF)

	soBytes, err := os.ReadFile(stdoutF.Name())
	if err != nil {
		t.Fatalf("read stdout temp: %v", err)
	}
	seBytes, err := os.ReadFile(stderrF.Name())
	if err != nil {
		t.Fatalf("read stderr temp: %v", err)
	}
	return code, bytes.NewBuffer(soBytes), bytes.NewBuffer(seBytes)
}

func TestSkillsInstall_WritesAllEmbeddedFiles(t *testing.T) {
	target := filepath.Join(t.TempDir(), "skills")
	code, _, _ := runCLI(t, "skills", "install", "--target", target)
	if code != 0 {
		t.Fatalf("skills install exit code = %d; want 0", code)
	}

	embedded := cli.Files()
	if len(embedded) == 0 {
		t.Fatal("embedded file set is empty; skills install would have nothing to write")
	}

	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatalf("read target dir: %v", err)
	}
	gotNames := map[string]bool{}
	for _, e := range entries {
		gotNames[e.Name()] = true
	}

	for name, want := range embedded {
		if !gotNames[name] {
			t.Errorf("missing %s in target", name)
			continue
		}
		got, err := os.ReadFile(filepath.Join(target, name))
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: on-disk content differs from embedded content (%d vs %d bytes)", name, len(got), len(want))
		}
	}

	// Make sure we didn't leave .tmp siblings around.
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

func TestSkillsInstall_OverwritesPriorContent(t *testing.T) {
	target := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	// Drop a stale copy of one skill with wrong content; install
	// should overwrite it.
	stale := filepath.Join(target, "build-enrichment-table.md")
	if err := os.WriteFile(stale, []byte("STALE\n"), 0o644); err != nil {
		t.Fatalf("seed stale: %v", err)
	}
	// Drop an unrelated file; install should leave it alone (this
	// is a "no merge but no nuke" property — install replaces named
	// files, doesn't reset the whole directory).
	keep := filepath.Join(target, "user-custom.md")
	if err := os.WriteFile(keep, []byte("CUSTOM\n"), 0o644); err != nil {
		t.Fatalf("seed keep: %v", err)
	}

	code, _, _ := runCLI(t, "skills", "install", "--target", target)
	if code != 0 {
		t.Fatalf("skills install exit code = %d; want 0", code)
	}

	got, err := os.ReadFile(stale)
	if err != nil {
		t.Fatalf("read post-install: %v", err)
	}
	if bytes.Equal(got, []byte("STALE\n")) {
		t.Error("stale content was not overwritten")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("unrelated user file was removed: %v", err)
	}
}

func TestSkillsInstall_TargetIsSymlinkErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions on Windows make this case noisy")
	}
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatalf("mkdir real: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	code, _, stderr := runCLI(t, "skills", "install", "--target", link)
	if code == 0 {
		t.Fatalf("expected non-zero exit when target is a symlink; got 0")
	}
	if !strings.Contains(stderr.String(), "symbolic link") {
		t.Errorf("stderr missing 'symbolic link' guidance; got %q", stderr.String())
	}
	// And no files should have been written through the link.
	entries, _ := os.ReadDir(realDir)
	if len(entries) != 0 {
		t.Errorf("symlink target was written into despite guard: %d entries", len(entries))
	}
}

func TestSkillsInstall_ReadOnlyDirErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("0o500 dir perms not meaningful on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses POSIX mode bits; skip read-only check")
	}
	parent := t.TempDir()
	target := filepath.Join(parent, "ro-skills")
	if err := os.MkdirAll(target, 0o500); err != nil {
		t.Fatalf("mkdir ro: %v", err)
	}
	defer os.Chmod(target, 0o755) //nolint:errcheck

	code, _, stderr := runCLI(t, "skills", "install", "--target", target)
	if code == 0 {
		t.Fatalf("expected non-zero exit when target is read-only; got 0")
	}
	if stderr.Len() == 0 {
		t.Error("expected stderr error message on read-only target")
	}
}

func TestSkillsUninstall_RequiresYesFlag(t *testing.T) {
	target := filepath.Join(t.TempDir(), "skills")
	// Install first so there's something to uninstall.
	if code, _, _ := runCLI(t, "skills", "install", "--target", target); code != 0 {
		t.Fatalf("setup install failed: %d", code)
	}

	// Without --yes: must error and leave the directory in place.
	code, _, stderr := runCLI(t, "skills", "uninstall", "--target", target)
	if code == 0 {
		t.Fatalf("expected non-zero exit without --yes; got 0")
	}
	if !strings.Contains(stderr.String(), "--yes") {
		t.Errorf("stderr missing --yes hint; got %q", stderr.String())
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("uninstall removed the dir without --yes: %v", err)
	}

	// With --yes: must succeed and remove the directory.
	code, _, _ = runCLI(t, "skills", "uninstall", "--target", target, "--yes")
	if code != 0 {
		t.Fatalf("uninstall --yes exit code = %d; want 0", code)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("uninstall --yes failed to remove target dir: stat err = %v", err)
	}
}

func TestSkillsUninstall_IdempotentWhenMissing(t *testing.T) {
	target := filepath.Join(t.TempDir(), "never-installed")
	code, _, _ := runCLI(t, "skills", "uninstall", "--target", target, "--yes")
	if code != 0 {
		t.Errorf("uninstall on missing dir should be idempotent; got exit code %d", code)
	}
}

func TestSkillsInstall_DefaultTargetUsesHomeDir(t *testing.T) {
	// Override HOME to a temp dir so we can assert default-target
	// resolution without touching the real ~/.claude.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	// XDG_CONFIG_HOME is unrelated to the skills install target
	// (which uses ~/.claude/plugins/, not XDG), but unset it for
	// robustness.
	t.Setenv("XDG_CONFIG_HOME", "")

	code, _, _ := runCLI(t, "skills", "install")
	if code != 0 {
		t.Fatalf("skills install exit code = %d; want 0", code)
	}
	want := filepath.Join(tmp, ".claude", "plugins", "moltable", "skills")
	entries, err := os.ReadDir(want)
	if err != nil {
		t.Fatalf("default install target %s not populated: %v", want, err)
	}
	if len(entries) == 0 {
		t.Errorf("default install target has no entries")
	}
}
