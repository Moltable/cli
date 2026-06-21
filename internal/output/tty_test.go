package output

import (
	"testing"
)

// withProbe wires a custom IsTerminal probe + resets cache. It returns
// a cleanup function so individual subtests can isolate themselves.
func withProbe(t *testing.T, isTerm bool) {
	t.Helper()
	ResetForTest(func() bool { return isTerm })
	t.Cleanup(func() { ResetForTest(nil) })
}

func TestIsTTY_TrueWhenTerminalAndNoOverrides(t *testing.T) {
	// Each env var: setting to empty via t.Setenv is the only way to
	// reliably override what the developer's shell happens to set.
	t.Setenv(envNoColor, "")
	t.Setenv(envMoltableNoColor, "")
	t.Setenv(envCI, "")
	withProbe(t, true)
	if !IsTTY() {
		t.Fatal("IsTTY = false, want true (terminal + no overrides)")
	}
}

func TestIsTTY_FalseWhenPiped(t *testing.T) {
	t.Setenv(envNoColor, "")
	t.Setenv(envMoltableNoColor, "")
	t.Setenv(envCI, "")
	withProbe(t, false) // simulate piped stdout
	if IsTTY() {
		t.Fatal("IsTTY = true, want false (stdout piped)")
	}
}

func TestIsTTY_FalseWhenNoColorSet(t *testing.T) {
	t.Setenv(envNoColor, "1")
	t.Setenv(envMoltableNoColor, "")
	t.Setenv(envCI, "")
	withProbe(t, true)
	if IsTTY() {
		t.Fatal("IsTTY = true, want false (NO_COLOR=1)")
	}
}

func TestIsTTY_FalseWhenMoltableNoColorSet(t *testing.T) {
	t.Setenv(envNoColor, "")
	t.Setenv(envMoltableNoColor, "1")
	t.Setenv(envCI, "")
	withProbe(t, true)
	if IsTTY() {
		t.Fatal("IsTTY = true, want false (MOLTABLE_NO_COLOR=1)")
	}
}

func TestIsTTY_FalseWhenCISet(t *testing.T) {
	t.Setenv(envNoColor, "")
	t.Setenv(envMoltableNoColor, "")
	t.Setenv(envCI, "true")
	withProbe(t, true)
	if IsTTY() {
		t.Fatal("IsTTY = true, want false (CI=true)")
	}
}

func TestIsTTY_CachedAcrossCalls(t *testing.T) {
	// Once IsTTY returns a value, changing the env should NOT change
	// the answer for the lifetime of the process — agents rely on a
	// stable value mid-run.
	t.Setenv(envNoColor, "")
	t.Setenv(envMoltableNoColor, "")
	t.Setenv(envCI, "")
	withProbe(t, true)
	first := IsTTY()
	if !first {
		t.Fatal("setup: expected IsTTY=true")
	}
	// Mutate env after the cache warmed.
	t.Setenv(envNoColor, "1")
	if IsTTY() != first {
		t.Fatal("IsTTY result changed after first call — cache broken")
	}
}

func TestComputeIsTTY_DirectInvocation(t *testing.T) {
	// computeIsTTY is the uncached path. Verifying it independently
	// lets us prove the env logic works without fighting the cache.
	t.Setenv(envNoColor, "")
	t.Setenv(envMoltableNoColor, "")
	t.Setenv(envCI, "")
	ResetForTest(func() bool { return true })
	t.Cleanup(func() { ResetForTest(nil) })
	if !computeIsTTY() {
		t.Errorf("computeIsTTY() = false, want true")
	}
	t.Setenv(envCI, "1")
	if computeIsTTY() {
		t.Errorf("computeIsTTY() with CI=1 = true, want false")
	}
}
