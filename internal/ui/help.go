package ui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/alecthomas/kong"
)

// RenderHelp is a kong.HelpPrinter that renders a gh-style organized
// help screen for the root command (sections: USAGE / CORE / ALIAS /
// ADDITIONAL / FLAGS / EXAMPLES / LEARN MORE). Sub-command help
// (`moltable auth --help`, `moltable run table --help`) falls through
// to kong.DefaultHelpPrinter — those leaves benefit more from Kong's
// detailed flag listings than from a grouped overview.
//
// Wire via:
//
//	kong.New(cli, kong.Help(ui.RenderHelp), kong.Groups{...})
//
// The Groups registration is what binds group keys (struct tag values
// like `group:"core"`) to the human titles ("CORE COMMANDS") we
// render here. Without it, our groupKey → title map below has to keep
// the titles in sync manually, which is fine because we own both
// sides — but using kong.Groups also makes `kong.DefaultHelpPrinter`
// render sub-tree help with the right group titles.
func RenderHelp(options kong.HelpOptions, ctx *kong.Context) error {
	// Selected() returns nil for the root command (root is the
	// implicit "no command picked" state). Anything non-nil is a
	// chosen subtree — that gets the default Kong layout, which
	// itemizes flags + positionals in detail.
	if ctx.Selected() != nil {
		return kong.DefaultHelpPrinter(options, ctx)
	}

	w := ctx.Stdout
	model := ctx.Model

	// USAGE — single line, conventional shape.
	fmt.Fprintln(w, sectionHeader("USAGE"))
	fmt.Fprintf(w, "  %s <command> <subcommand> [flags]\n\n", model.Name)

	// Walk the root's children, partition by group key. Kong promises
	// Children is ordered as declared in the struct, so within a
	// group we get a stable, intuitive order.
	groups := groupRootChildren(model.Node.Children)

	// Section order is INTENTIONAL — CORE first (the data + execution
	// surface a new user reaches for), ALIAS next (so the bare `watch`
	// is discoverable next to the canonical `run watch`), then
	// ADDITIONAL for utility/meta verbs that don't carry the daily
	// flow. We don't render an "(empty)" section when a group has
	// zero entries.
	type sectionDef struct {
		key, title string
	}
	sections := []sectionDef{
		{"core", "CORE COMMANDS"},
		{"alias", "ALIAS COMMANDS"},
		{"additional", "ADDITIONAL COMMANDS"},
	}
	// Plus any unrecognized group keys, alphabetized, so a future
	// `group:"foo"` doesn't silently vanish from help when someone
	// adds it to a verb but forgets to register it here.
	seen := map[string]bool{"core": true, "alias": true, "additional": true}
	var extras []string
	for key := range groups {
		if !seen[key] {
			extras = append(extras, key)
		}
	}
	sort.Strings(extras)
	for _, k := range extras {
		sections = append(sections, sectionDef{k, strings.ToUpper(k) + " COMMANDS"})
	}

	// Also catch group=="" (unannotated commands) so we don't drop them
	// silently if someone adds a verb without a group tag.
	if len(groups[""]) > 0 {
		sections = append(sections, sectionDef{"", "OTHER COMMANDS"})
	}

	for _, s := range sections {
		cmds := groups[s.key]
		if len(cmds) == 0 {
			continue
		}
		fmt.Fprintln(w, sectionHeader(s.title))
		// Compute longest name in this section for column alignment.
		// We pad with spaces; the visible width is what matters (color
		// escapes don't count toward terminal column math).
		maxNameLen := 0
		for _, c := range cmds {
			if l := len(c.Name); l > maxNameLen {
				maxNameLen = l
			}
		}
		for _, c := range cmds {
			pad := strings.Repeat(" ", maxNameLen-len(c.Name))
			fmt.Fprintf(w, "  %s:%s  %s\n", Bold(c.Name), pad, c.Help)
		}
		fmt.Fprintln(w)
	}

	// FLAGS — only the global flags on the root. Sub-tree flag
	// listings stay with the default printer.
	if len(model.Node.Flags) > 0 {
		fmt.Fprintln(w, sectionHeader("FLAGS"))
		// Build display strings first so we can right-align the
		// description column the same way kong.DefaultHelpPrinter
		// does for sub-trees — keeps the look consistent.
		type flagRow struct{ left, right string }
		rows := make([]flagRow, 0, len(model.Node.Flags))
		maxLeft := 0
		for _, f := range model.Node.Flags {
			if f.Hidden {
				continue
			}
			left := flagLeft(f)
			if len(left) > maxLeft {
				maxLeft = len(left)
			}
			rows = append(rows, flagRow{left: left, right: f.Help})
		}
		for _, r := range rows {
			pad := strings.Repeat(" ", maxLeft-len(r.left))
			fmt.Fprintf(w, "  %s%s  %s\n", r.left, pad, Hint(r.right))
		}
		fmt.Fprintln(w)
	}

	// EXAMPLES — canonical first-week workflows. Kept short; if a
	// user wants the full reference they read the per-command help.
	fmt.Fprintln(w, sectionHeader("EXAMPLES"))
	fmt.Fprintln(w, "  $ moltable auth login")
	fmt.Fprintln(w, "  $ moltable workbook create \"Lead research\"")
	fmt.Fprintln(w, "  $ moltable table create --workbook wkb_X --name \"Leads\"")
	fmt.Fprintln(w, "  $ moltable row import --table tbl_X --csv ./leads.csv")
	fmt.Fprintln(w, "  $ moltable run table tbl_X --watch --json")
	fmt.Fprintln(w)

	// LEARN MORE — discoverability footer. gh's pattern; users expect
	// it at the bottom of the top-level help.
	fmt.Fprintln(w, sectionHeader("LEARN MORE"))
	fmt.Fprintf(w, "  Use %s for details on a specific command.\n", Bold("`moltable <command> --help`"))
	fmt.Fprintln(w, "  Issues:   "+Accent("https://github.com/moltable/cli/issues"))
	fmt.Fprintln(w, "  Releases: "+Accent("https://github.com/moltable/cli/releases"))

	return nil
}

// sectionHeader applies the canonical gh-style title formatting:
// uppercase + bold. Color-gated by the ui package's TTY check.
func sectionHeader(s string) string {
	return Bold(s)
}

// flagLeft renders a flag's invocation form ("-h, --help" or
// "--api-key=STRING") for the FLAGS section. Mirrors how Kong's own
// help renders flag descriptors so our output looks at-home next to
// `moltable auth --help` (which still goes through kong.DefaultHelpPrinter).
func flagLeft(f *kong.Flag) string {
	var parts []string
	if f.Short != 0 {
		parts = append(parts, fmt.Sprintf("-%c", f.Short))
	}
	long := "--" + f.Name
	// Boolean flags don't take a value; everything else does.
	if !isBoolFlag(f) {
		long += "=" + placeholder(f)
	}
	parts = append(parts, long)
	return strings.Join(parts, ", ")
}

// isBoolFlag — Kong's Flag.IsBool() exists but is on a parent type;
// inspect the underlying tag's type field instead.
func isBoolFlag(f *kong.Flag) bool {
	if f.Target.IsValid() {
		return f.Target.Kind().String() == "bool"
	}
	return false
}

// placeholder picks the metavar shown after `--flag=`. Kong stores
// this on the Tag's PlaceHolder field when set explicitly; fall back
// to the default Kong uses ("STRING" / "INT" / etc.) by uppercasing
// the kind.
func placeholder(f *kong.Flag) string {
	if f.PlaceHolder != "" {
		return f.PlaceHolder
	}
	if f.Target.IsValid() {
		return strings.ToUpper(f.Target.Kind().String())
	}
	return "VALUE"
}

// groupRootChildren partitions the root command's children by their
// `group:"…"` tag value. Used by RenderHelp; exposed so tests can
// assert on the partition shape without re-running Kong.
func groupRootChildren(children []*kong.Node) map[string][]*kong.Node {
	out := map[string][]*kong.Node{}
	for _, c := range children {
		if c.Hidden {
			continue
		}
		key := ""
		if c.Group != nil {
			key = c.Group.Key
		}
		out[key] = append(out[key], c)
	}
	return out
}
