package output

import (
	"bytes"
	stderrors "errors"
	"strings"
	"testing"
)

func TestPrint_BareStructNoJQ(t *testing.T) {
	type thing struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	var buf bytes.Buffer
	if err := Print(&buf, thing{ID: "t1", Name: "X"}, ""); err != nil {
		t.Fatalf("Print: %v", err)
	}
	got := buf.String()
	want := `{"id":"t1","name":"X"}` + "\n"
	if got != want {
		t.Errorf("Print = %q, want %q", got, want)
	}
}

func TestPrint_BareStructHasExactlyOneTrailingNewline(t *testing.T) {
	var buf bytes.Buffer
	if err := Print(&buf, map[string]int{"a": 1}, ""); err != nil {
		t.Fatalf("Print: %v", err)
	}
	out := buf.Bytes()
	if len(out) == 0 || out[len(out)-1] != '\n' {
		t.Errorf("missing trailing newline; got %q", out)
	}
	// Last two bytes shouldn't both be newlines.
	if len(out) >= 2 && out[len(out)-2] == '\n' {
		t.Errorf("trailing whitespace bloat: %q", out)
	}
}

func TestPrint_JQScalarFromObject(t *testing.T) {
	// `--jq '.id'` on {"id":"X"} writes "X" (the JSON-encoded string,
	// quoted) on its own line — same as `jq -c '.id'`.
	var buf bytes.Buffer
	err := Print(&buf, map[string]string{"id": "X", "name": "Y"}, ".id")
	if err != nil {
		t.Fatalf("Print: %v", err)
	}
	want := `"X"` + "\n"
	if got := buf.String(); got != want {
		t.Errorf("Print = %q, want %q", got, want)
	}
}

func TestPrint_JQOneIDPerLineFromList(t *testing.T) {
	// `--jq '.[]|.id'` on a list emits one ID per line.
	items := []map[string]string{
		{"id": "a"},
		{"id": "b"},
		{"id": "c"},
	}
	var buf bytes.Buffer
	if err := Print(&buf, items, ".[]|.id"); err != nil {
		t.Fatalf("Print: %v", err)
	}
	want := "\"a\"\n\"b\"\n\"c\"\n"
	if got := buf.String(); got != want {
		t.Errorf("Print = %q, want %q", got, want)
	}
}

func TestPrint_JQObjectShape(t *testing.T) {
	// jq object construction yields a compact JSON object per line.
	items := []map[string]any{
		{"id": "a", "name": "Alpha"},
		{"id": "b", "name": "Beta"},
	}
	var buf bytes.Buffer
	if err := Print(&buf, items, ".[]|{id, name}"); err != nil {
		t.Fatalf("Print: %v", err)
	}
	// gojq's encoder writes keys in the order they appear in the
	// object literal, matching jq.
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d (%q)", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], `"id":"a"`) {
		t.Errorf("line[0] = %q", lines[0])
	}
}

func TestPrint_JQParseErrorTyped(t *testing.T) {
	var buf bytes.Buffer
	err := Print(&buf, map[string]string{"a": "b"}, "this is not valid jq @@@")
	if err == nil {
		t.Fatal("Print: want JQParseError, got nil")
	}
	var pe *JQParseError
	if !stderrors.As(err, &pe) {
		t.Fatalf("err type = %T, want *JQParseError (%v)", err, err)
	}
	if pe.Expr != "this is not valid jq @@@" {
		t.Errorf("JQParseError.Expr = %q", pe.Expr)
	}
}

func TestPrint_JQRuntimeErrorTyped(t *testing.T) {
	// `.foo` on a non-object number triggers a jq evaluation error.
	var buf bytes.Buffer
	err := Print(&buf, 42, ".foo")
	if err == nil {
		t.Fatal("Print: want runtime error, got nil")
	}
	var re *JQRuntimeError
	if !stderrors.As(err, &re) {
		t.Fatalf("err type = %T, want *JQRuntimeError (%v)", err, err)
	}
}

func TestPrint_NumberPreservedThroughJQ(t *testing.T) {
	// Round-tripping through interface{} keeps integers small enough
	// to be exact in float64. Make sure we emit "7" not "7.0".
	var buf bytes.Buffer
	if err := Print(&buf, map[string]int{"n": 7}, ".n"); err != nil {
		t.Fatalf("Print: %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != "7" {
		t.Errorf("Print = %q, want %q", got, "7")
	}
}

func TestPrint_BareEnvelopeFree(t *testing.T) {
	// The --json contract requires no envelope (no {"data":...}). Make
	// sure the raw payload IS the output.
	var buf bytes.Buffer
	if err := Print(&buf, []int{1, 2, 3}, ""); err != nil {
		t.Fatalf("Print: %v", err)
	}
	got := strings.TrimSpace(buf.String())
	if got != "[1,2,3]" {
		t.Errorf("Print = %q, want bare array `[1,2,3]`", got)
	}
}

// errWriter always fails on Write. Used to confirm io errors
// propagate cleanly (no double-wrap, no swallowed nil).
type errWriter struct{}

func (errWriter) Write(p []byte) (int, error) { return 0, stderrors.New("write fail") }

func TestPrint_PropagatesWriterError(t *testing.T) {
	err := Print(errWriter{}, map[string]string{"a": "b"}, "")
	if err == nil {
		t.Fatal("Print: want io error, got nil")
	}
	if !strings.Contains(err.Error(), "write fail") {
		t.Errorf("err = %v, want it to mention `write fail`", err)
	}
}
