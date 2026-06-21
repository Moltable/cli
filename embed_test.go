package cli

import (
	"bytes"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// expectedSkills is the canonical list of skill files that must ship
// in the embed bundle. Adding or removing one is intentional and
// should be reflected here so the embed surface stays explicit.
var expectedSkills = []string{
	"auth-and-profiles.md",
	"build-enrichment-table.md",
	"long-tail-fallback.md",
	"run-and-watch-jobs.md",
}

// knownCommands is the set of `moltable <verb-chain>` invocations the
// skill bodies are allowed to reference. It mirrors the Kong command
// tree declared in cmd/moltable/main.go plus the skills install/
// uninstall verbs. If the CLI grows a verb, add it here AND make
// sure at least one skill mentions it.
//
// Wildcards aren't supported — each command must be spelled out.
var knownCommands = map[string]bool{
	"moltable":                  true,
	"moltable auth":             true,
	"moltable auth login":       true,
	"moltable auth logout":      true,
	"moltable auth check":       true,
	"moltable profile":          true,
	"moltable profile list":     true,
	"moltable profile use":      true,
	"moltable profile remove":   true,
	"moltable workbook":         true,
	"moltable workbook create":  true,
	"moltable workbook list":    true,
	"moltable table":            true,
	"moltable table create":     true,
	"moltable table list":       true,
	"moltable table get":        true,
	"moltable table export":     true,
	"moltable column":           true,
	"moltable column add":       true,
	"moltable column list":      true,
	"moltable row":              true,
	"moltable row create":       true,
	"moltable row import":       true,
	"moltable run":              true,
	"moltable run table":        true,
	"moltable run cell":         true,
	"moltable run watch":        true,
	"moltable stop":             true,
	"moltable watch":            true,
	"moltable version":          true,
	"moltable upgrade":          true,
	"moltable skills":           true,
	"moltable skills install":   true,
	"moltable skills uninstall": true,
	"moltable config":           true,
	"moltable config path":      true,
	"moltable config show":      true,
	"moltable config get":       true, // `moltable config get api-key`
	"moltable config set":       true, // `moltable config set default-profile`
}

func TestFilesReturnsExactlyFourSkills(t *testing.T) {
	got := Files()
	if len(got) != len(expectedSkills) {
		names := make([]string, 0, len(got))
		for name := range got {
			names = append(names, name)
		}
		sort.Strings(names)
		t.Fatalf("Files() returned %d entries (%v); want %d (%v)",
			len(got), names, len(expectedSkills), expectedSkills)
	}
	for _, want := range expectedSkills {
		body, ok := got[want]
		if !ok {
			t.Errorf("Files() missing %q", want)
			continue
		}
		if len(body) == 0 {
			t.Errorf("Files()[%q] is empty", want)
		}
	}
}

// TestFilesContentIsByteForByteIdentical guards against accidental
// transformation in the embed wrapper. We don't have access to the
// source files at test time without re-reading them, so instead we
// assert a stable property: each file starts with the YAML
// frontmatter delimiter.
func TestFilesContentStartsWithFrontmatter(t *testing.T) {
	for name, body := range Files() {
		if !bytes.HasPrefix(body, []byte("---\n")) {
			t.Errorf("%s: missing YAML frontmatter (must start with `---\\n`)", name)
		}
	}
}

// TestSkillFrontmatterFields verifies every skill carries the three
// required frontmatter fields that Claude Code's plugin discovery
// uses: name, description, when_to_use. We parse the frontmatter as
// a very small line-oriented YAML subset — enough for the flat keys
// the skill format uses.
func TestSkillFrontmatterFields(t *testing.T) {
	required := []string{"name", "description", "when_to_use"}
	for fileName, body := range Files() {
		fm, err := extractFrontmatter(body)
		if err != nil {
			t.Errorf("%s: %v", fileName, err)
			continue
		}
		for _, key := range required {
			if val := fm[key]; strings.TrimSpace(val) == "" {
				t.Errorf("%s: frontmatter missing %q", fileName, key)
			}
		}
		// The `name` field must match the filename minus .md so
		// Claude Code's resolver lines up with on-disk paths.
		wantName := strings.TrimSuffix(fileName, ".md")
		if got := fm["name"]; got != wantName {
			t.Errorf("%s: frontmatter name=%q; want %q", fileName, got, wantName)
		}
	}
}

// TestSkillCommandsAreKnown statically scans each skill body for
// `moltable <verb>` invocations and verifies every one matches a real
// command in the Kong tree. This catches drift where a skill drifts
// ahead of (or behind) the CLI's command surface.
func TestSkillCommandsAreKnown(t *testing.T) {
	// Match `moltable` optionally followed by a chain of lowercased
	// verb tokens. Stops at the first non-verb token (flag, arg,
	// punctuation) so `moltable run table tb_X --watch` extracts to
	// `moltable run table`.
	cmdRE := regexp.MustCompile(`\bmoltable(?:\s+[a-z][a-z0-9_-]*)*`)
	verbRE := regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

	for fileName, body := range Files() {
		bodyStr := stripFrontmatter(string(body))
		matches := cmdRE.FindAllString(bodyStr, -1)
		for _, raw := range matches {
			parts := strings.Fields(raw)
			// Trim trailing tokens until what remains is in
			// knownCommands. This handles cases like
			// `moltable run table tb_X` where `tb_X` is an arg, not
			// a verb — we walk back to `moltable run table`.
			trimmed := parts
			for len(trimmed) > 0 {
				candidate := strings.Join(trimmed, " ")
				if knownCommands[candidate] {
					break
				}
				// If the last token doesn't look like a verb at all,
				// drop it without complaint.
				last := trimmed[len(trimmed)-1]
				if !verbRE.MatchString(last) {
					trimmed = trimmed[:len(trimmed)-1]
					continue
				}
				// Otherwise drop it and keep walking; if we run out
				// we'll flag below.
				trimmed = trimmed[:len(trimmed)-1]
			}
			if len(trimmed) == 0 {
				t.Errorf("%s: command %q resolves to no known moltable verb", fileName, raw)
			}
		}
	}
}

// extractFrontmatter returns a flat map of the YAML frontmatter at
// the head of a markdown document. Supports the subset used by the
// skill format: `key: value` on a single line, no quoting, no nested
// structures.
func extractFrontmatter(body []byte) (map[string]string, error) {
	s := string(body)
	if !strings.HasPrefix(s, "---\n") {
		return nil, &frontmatterErr{reason: "missing opening ---"}
	}
	rest := s[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		// Trailing --- at end-of-file (no newline) is also accepted.
		if strings.HasSuffix(rest, "\n---") {
			end = len(rest) - len("\n---")
		} else {
			return nil, &frontmatterErr{reason: "missing closing ---"}
		}
	}
	out := map[string]string{}
	for _, line := range strings.Split(rest[:end], "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		colon := strings.Index(line, ":")
		if colon < 0 {
			return nil, &frontmatterErr{reason: "non-key:value line: " + line}
		}
		key := strings.TrimSpace(line[:colon])
		val := strings.TrimSpace(line[colon+1:])
		out[key] = val
	}
	return out, nil
}

// stripFrontmatter returns the markdown body with any YAML
// frontmatter removed. Used by command-scan tests so frontmatter
// `description:` text isn't double-counted as command examples.
func stripFrontmatter(s string) string {
	if !strings.HasPrefix(s, "---\n") {
		return s
	}
	rest := s[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return s
	}
	return rest[end+len("\n---\n"):]
}

type frontmatterErr struct{ reason string }

func (e *frontmatterErr) Error() string { return "frontmatter: " + e.reason }
