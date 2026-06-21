// Package output (json.go) — gh-style JSON + jq output.
//
// The CLI's machine-readable contract is:
//
//   - `--json` alone: bare json.Marshal of the data, no envelope, no
//     trailing newline pollution. `moltable table get t1 --json` emits
//     `{"id":"t1",...}` ending with exactly one newline.
//   - `--json --jq <expr>`: feed the JSON through gojq, emit each
//     result on its own line in JSON encoding (matches `jq -c`).
//     Scalars are emitted as scalar JSON ("X" → `"X"`, 7 → `7`).
//
// We use github.com/itchyny/gojq instead of shelling out to `jq` for
// two reasons: zero runtime dependency on the user's environment (jq
// isn't installed everywhere — especially in minimal Docker images
// agents run in), and gojq matches jq's filter semantics close enough
// for our use cases (data shaping, not arithmetic).
//
// Output goes to an io.Writer the caller supplies — the actual
// command bodies pass os.Stdout. Surfacing the writer makes JSON
// tests trivial: write to a bytes.Buffer and assert on the bytes.
package output

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/itchyny/gojq"
)

// JQParseError signals a syntactically invalid jq expression. The
// command layer catches this and renders it as an InvalidInputError
// so the user sees a hint pointing at --jq.
type JQParseError struct {
	Expr string
	Err  error
}

func (e *JQParseError) Error() string {
	return fmt.Sprintf("output: parse --jq %q: %v", e.Expr, e.Err)
}

func (e *JQParseError) Unwrap() error { return e.Err }

// JQRuntimeError signals a jq expression that parsed but failed
// during evaluation (e.g. `.foo` on a number).
type JQRuntimeError struct {
	Expr string
	Err  error
}

func (e *JQRuntimeError) Error() string {
	return fmt.Sprintf("output: evaluate --jq %q: %v", e.Expr, e.Err)
}

func (e *JQRuntimeError) Unwrap() error { return e.Err }

// Print writes v to w as JSON. When jqExpr is empty, the output is a
// single json.Marshal(v) followed by one newline. When jqExpr is set,
// v is marshaled to JSON, decoded to interface{}, fed through gojq,
// and each result is written on its own line.
//
// The function never adds trailing whitespace beyond the final
// newline — agents grep/awk this output and stray whitespace would
// break their pipelines.
//
// Errors:
//   - *JQParseError when jqExpr is malformed.
//   - *JQRuntimeError when the parsed query errors during evaluation.
//   - Any io error returned by w is propagated unwrapped.
//
// `v` may be any value json.Marshal supports. For struct types, struct
// tags (`json:"..."`) determine the field names — same as standard
// library encoding/json.
func Print(w io.Writer, v interface{}, jqExpr string) error {
	// Fast path: no jq → straight marshal + newline.
	if jqExpr == "" {
		raw, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("output: marshal: %w", err)
		}
		return writeLine(w, raw)
	}

	query, err := gojq.Parse(jqExpr)
	if err != nil {
		return &JQParseError{Expr: jqExpr, Err: err}
	}

	// gojq operates on interface{} values (the same shape the stdlib
	// produces when decoding into interface{}). Round-trip v through
	// JSON so struct tags + json.Marshaler implementations are
	// respected — gojq won't introspect Go field names directly.
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("output: marshal: %w", err)
	}
	var decoded interface{}
	// UseNumber would preserve int64 precision but breaks gojq's
	// arithmetic. We accept the float64 representation jq itself
	// also uses.
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return fmt.Errorf("output: round-trip decode: %w", err)
	}

	iter := query.Run(decoded)
	for {
		val, ok := iter.Next()
		if !ok {
			return nil
		}
		if errVal, isErr := val.(error); isErr {
			return &JQRuntimeError{Expr: jqExpr, Err: errVal}
		}
		// jq emits each result independently; we match `jq -c` and
		// write compact JSON per line.
		out, err := json.Marshal(val)
		if err != nil {
			return fmt.Errorf("output: marshal jq result: %w", err)
		}
		if err := writeLine(w, out); err != nil {
			return err
		}
	}
}

// writeLine writes data followed by exactly one '\n'. Returning the
// io error unwrapped keeps callers free to attach their own context.
func writeLine(w io.Writer, data []byte) error {
	if _, err := w.Write(data); err != nil {
		return err
	}
	_, err := w.Write([]byte{'\n'})
	return err
}
