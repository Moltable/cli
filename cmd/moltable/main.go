// Command moltable is the moltable CLI entry point. Builds a Kong
// command tree, resolves credentials via the tri-layer flag → env →
// config chain, and dispatches to per-verb handlers in the same
// package.
//
// Top-level shape (`<noun> <verb>`):
//
//	moltable auth     login | logout | check
//	moltable profile  list | use | remove
//	moltable workbook create | list
//	moltable table    create | list | get | export
//	moltable view     list | get | search
//	moltable column   add | list
//	moltable row      create | import
//	moltable run      table | cell | watch
//	moltable stop
//	moltable watch    (alias for `moltable run watch`)
//	moltable version
//	moltable upgrade
//	moltable skills   install | uninstall
//	moltable config   path | show | get | set
//
// Global flags:
//
//	--api-key <molt_...>  override the active credential for one call
//	--profile <name>      override which TOML profile to use
//	--config <path>       override the config file location (testing)
//	--dev                 target local API on https://localhost:8080
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/kong"

	"github.com/moltable/cli/internal/auth"
	"github.com/moltable/cli/internal/config"
	clierrors "github.com/moltable/cli/internal/errors"
	"github.com/moltable/cli/internal/ui"
	"github.com/moltable/cli/internal/updater"
	clversion "github.com/moltable/cli/internal/version"
)

// version is the Kong-vars handle for `${version}` interpolation in
// help text. The canonical source of truth is
// internal/version.BinaryVersion (overridden by goreleaser at build
// time via ldflags); we mirror it here so Kong's help renderer
// matches what `moltable version` prints. Kept as a `var` so
// existing call sites that took its address keep compiling.
var version = clversion.BinaryVersion

// CLI is the Kong root struct. Every sub-struct corresponds to a
// noun; every field on those sub-structs is a verb.
type CLI struct {
	// Global flags applied to every subcommand. Kept on the root so
	// Kong inherits them without per-command duplication.
	APIKey  string `name:"api-key" help:"Override the active API key for this call (must begin with molt_)."`
	Profile string `name:"profile" help:"Override which named profile to use from config.toml."`
	Config  string `name:"config" help:"Override config file path (defaults to $XDG_CONFIG_HOME/moltable/config.toml)." type:"path"`
	// Dev mode toggles three behaviors at once: api_base defaults to
	// https://localhost:8080 (overridable via MOLTABLE_API_BASE), TLS
	// verification is skipped (needed for the local devcerts the API
	// serves), and the user-agent is suffixed `+dev` so server-side
	// access logs can distinguish dev traffic. Useful for full-stack
	// smoke testing without touching prod. Mirror via MOLTABLE_DEV=1
	// so you can `export MOLTABLE_DEV=1` once per terminal.
	Dev bool `name:"dev" env:"MOLTABLE_DEV" help:"Dev mode: target local API on https://localhost:8080 and skip TLS verification."`

	// Top-level commands. `group:"…"` tags drive the gh-style help
	// sections rendered by internal/ui.RenderHelp. Keys are
	// human-meaningful so a quick `grep group:"core"` shows the
	// promoted surface; titles + descriptions are registered via
	// kong.Groups{...} at parser construction below.
	Auth     AuthCmd     `cmd:"" group:"core"       help:"Authenticate via browser handoff and manage credentials."`
	Workbook WorkbookCmd `cmd:"" group:"core"       help:"Manage workbooks (top-level containers for tables)."`
	Table    TableCmd    `cmd:"" group:"core"       help:"Create, inspect, and export tables."`
	View     ViewCmd     `cmd:"" group:"core"       help:"List views on a table and search cells inside a view."`
	Column   ColumnCmd   `cmd:"" group:"core"       help:"Add and list columns on a table."`
	Row      RowCmd      `cmd:"" group:"core"       help:"Create rows and import data into tables."`
	Run      RunCmd      `cmd:"" group:"core"       help:"Trigger executions over tables, rows, or single cells."`

	// Convenience alias for the most common verb (`moltable run watch`
	// → `moltable watch`). Surfaces under ALIAS COMMANDS in help so
	// users discover both the bare form and the canonical noun-verb.
	Watch WatchCmd `cmd:"" group:"alias"      help:"Stream execution events from the server (alias for \"run watch\")."`

	// Less-common verbs: stop, profile management, config, install
	// helpers, version, upgrade. None of these are first-touch on a
	// fresh CLI; the alphabetical CORE block hides them from immediate
	// view but they stay one help-screen away.
	Stop     StopCmd    `cmd:"" group:"additional" help:"Stop a running execution."`
	Profile_ ProfileCmd `cmd:"" name:"profile" group:"additional" help:"Inspect and switch named credential profiles."`
	ConfigCm ConfigCmd  `cmd:"" name:"config"  group:"additional" help:"Inspect the active config file."`
	Skills   SkillsCmd  `cmd:"" group:"additional" help:"Install the bundled Claude Code skills."`
	Version  VersionCmd `cmd:"" group:"additional" help:"Print the CLI version."`
	Upgrade  UpgradeCmd `cmd:"" group:"additional" help:"Self-update to the latest release."`
}

// --- Auth ---------------------------------------------------------

// AuthCmd groups the credential lifecycle commands.
type AuthCmd struct {
	Login  AuthLoginCmd  `cmd:"" help:"Authenticate via browser handoff and save the resulting key to a profile."`
	Logout AuthLogoutCmd `cmd:"" help:"Forget a profile and revoke its key on the server."`
	Check  AuthCheckCmd  `cmd:"" help:"Print which credential is active and which profile it came from."`
}

// AuthLoginCmd wires `moltable auth login`. The body lives in auth.go
// so the command structs stay co-located on main.go but the wiring
// (handoff dance, profile write, /v1/me lookup) is testable in isolation.
//
// The profile name the login flow writes to is sourced from the global
// `--profile` flag — same flag the rest of the CLI uses to pick which
// profile to read. When the flag is unset, login uses "default".
type AuthLoginCmd struct {
	// Label overrides the auto-detected `<hostname> · <date>` device
	// label sent to the server at handoff init. Useful for pinning a
	// stable name across re-installs ("Production Deploy Key") or on
	// shared machines where the hostname isn't meaningful. Honors
	// MOLTABLE_NO_HOSTNAME as the privacy escape hatch — when set,
	// no label is sent and the server falls back to a date-only key
	// name.
	Label string `name:"label" help:"Override the device label shown in Settings → API Keys (defaults to hostname + date)."`
}

// AuthLogoutCmd wires `moltable auth logout`. Body in auth.go. The
// profile to remove comes from the global `--profile` flag; when unset
// the current default profile is removed.
type AuthLogoutCmd struct{}

// AuthCheckCmd wires `moltable auth check`. Body in auth.go.
type AuthCheckCmd struct {
	JSON bool `name:"json" help:"Emit JSON instead of human-readable output."`
}

// --- Profile ------------------------------------------------------

// ProfileCmd groups profile inspection / switching commands.
type ProfileCmd struct {
	List   ProfileListCmd   `cmd:"" help:"List named profiles in the config."`
	Use    ProfileUseCmd    `cmd:"" help:"Set the default profile."`
	Remove ProfileRemoveCmd `cmd:"" help:"Remove a named profile."`
}

// ProfileListCmd, ProfileUseCmd, ProfileRemoveCmd — bodies live in
// profile.go, co-located with their tests. The flag definitions stay
// here so Kong's help output and the tree shape remain readable in
// one file.
type ProfileListCmd struct {
	JSON bool `name:"json" help:"Emit JSON instead of human-readable output."`
}

type ProfileUseCmd struct {
	Name string `arg:"" name:"name" help:"Profile to make the default."`
}

type ProfileRemoveCmd struct {
	Name string `arg:"" name:"name" help:"Profile to remove."`
}

// --- Workbook ------------------------------------------------------

// WorkbookCmd groups the workbook CRUD verbs. Verb bodies live in
// workbook.go so this file remains the single Kong-shape source of
// truth.
type WorkbookCmd struct {
	Create WorkbookCreateCmd `cmd:"" help:"Create a workbook."`
	List   WorkbookListCmd   `cmd:"" help:"List workbooks."`
}

// WorkbookCreateCmd wires `moltable workbook create <name>`. Body in
// workbook.go.
type WorkbookCreateCmd struct {
	Name string `arg:"" name:"name" help:"Workbook name."`
	JSON bool   `name:"json" help:"Emit JSON instead of human-readable output."`
	JQ   string `name:"jq" help:"Filter --json output through this jq expression."`
}

// WorkbookListCmd wires `moltable workbook list`. Body in workbook.go.
type WorkbookListCmd struct {
	JSON bool   `name:"json" help:"Emit JSON instead of human-readable output."`
	JQ   string `name:"jq" help:"Filter --json output through this jq expression."`
}

// --- Table ---------------------------------------------------------

// TableCmd groups the table CRUD + export verbs. Verb bodies live in
// table.go.
type TableCmd struct {
	Create TableCreateCmd `cmd:"" help:"Create a table inside a workbook."`
	List   TableListCmd   `cmd:"" help:"List tables (optionally filtered by workbook)."`
	Get    TableGetCmd    `cmd:"" help:"Fetch a single table by id."`
	Export TableExportCmd `cmd:"" help:"Export a table to CSV / JSON."`
}

// TableCreateCmd wires `moltable table create --workbook <id> --name <name>`.
// `--workbook` is required and travels in the POST body (not the path),
// matching the API's POST /v1/tables shape.
type TableCreateCmd struct {
	Workbook string `name:"workbook" required:"" help:"Workbook ID to create the table inside."`
	Name     string `name:"name" required:"" help:"Table name."`
	JSON     bool   `name:"json" help:"Emit JSON instead of human-readable output."`
	JQ       string `name:"jq" help:"Filter --json output through this jq expression."`
}

// TableListCmd wires `moltable table list [--workbook <id>]`. When
// --workbook is set we hit the per-workbook list endpoint
// (/v1/workbooks/s/{shortId}/tables); otherwise the org-scoped
// /v1/tables list.
type TableListCmd struct {
	Workbook string `name:"workbook" help:"Filter to a single workbook."`
	JSON     bool   `name:"json" help:"Emit JSON instead of human-readable output."`
	JQ       string `name:"jq" help:"Filter --json output through this jq expression."`
}

// TableGetCmd wires `moltable table get <id>`. Body in table.go.
type TableGetCmd struct {
	ID   string `arg:"" name:"id" help:"Table ID."`
	JSON bool   `name:"json" help:"Emit JSON instead of human-readable output."`
	JQ   string `name:"jq" help:"Filter --json output through this jq expression."`
}

// TableExportCmd wires `moltable table export <id> --format csv|json`.
// `--format` picks the wire format the API speaks; `-o file` redirects
// the bytes to disk (otherwise stdout). The `--json` flag is distinct:
// it controls whether the summary line (rows written, bytes, etc.) is
// emitted as machine-readable JSON. Body in table.go.
type TableExportCmd struct {
	ID     string `arg:"" name:"id" help:"Table ID."`
	Format string `name:"format" required:"" enum:"csv,json" help:"Export format: csv | json."`
	Out    string `name:"out" short:"o" help:"Write to this path instead of stdout."`
	JSON   bool   `name:"json" help:"Emit a machine-readable summary instead of the human one."`
	JQ     string `name:"jq" help:"Filter --json output through this jq expression."`
}

// --- View ----------------------------------------------------------

// ViewCmd groups the view verbs. Verb bodies live in view.go.
//
// Why `view search` and not `table search`: the search is scoped to a
// view's filter, not the bare table — the view is the unit of work the
// caller addresses, and parking the verb under `view` keeps the noun
// hierarchy aligned with the data model (view → cells, not table →
// cells). It also leaves `moltable search …` open for the future home-
// search wrapper (workbook/folder/table names, distinct endpoint).
type ViewCmd struct {
	List   ViewListCmd   `cmd:"" help:"List saved views on a table."`
	Get    ViewGetCmd    `cmd:"" help:"Fetch a single view by id."`
	Search ViewSearchCmd `cmd:"" help:"Substring search across cells inside a view's filtered row set."`
}

// ViewListCmd wires `moltable view list --table <id>`. Body in view.go.
type ViewListCmd struct {
	Table string `name:"table" required:"" help:"Table ID whose views to list."`
	JSON  bool   `name:"json" help:"Emit JSON instead of human-readable output."`
	JQ    string `name:"jq" help:"Filter --json output through this jq expression."`
}

// ViewGetCmd wires `moltable view get --table <id> <viewId>`. Body in view.go.
type ViewGetCmd struct {
	Table string `name:"table" required:"" help:"Table ID the view belongs to."`
	ID    string `arg:"" name:"id" help:"View ID."`
	JSON  bool   `name:"json" help:"Emit JSON instead of human-readable output."`
	JQ    string `name:"jq" help:"Filter --json output through this jq expression."`
}

// ViewSearchCmd wires `moltable view search --table <id> --view <id> <query>
// [--limit N]`. Body in view.go.
//
// `query` is a positional so the most common shape is the shortest:
// `moltable view search --table tb_x --view vw_y linkedin`. The server
// enforces a 512-rune cap and rejects NUL; the CLI re-checks for empty
// to fail fast before the round-trip.
type ViewSearchCmd struct {
	Table string `name:"table" required:"" help:"Table ID the view belongs to."`
	View  string `name:"view" required:"" help:"View ID to scope the search to (use 'view list' to enumerate)."`
	Query string `arg:"" name:"query" help:"Substring to search for (case-insensitive, max 512 chars)."`
	Limit int    `name:"limit" help:"Reserved for future per-page semantics; server returns up to 5000 matched cells regardless."`
	JSON  bool   `name:"json" help:"Emit JSON instead of human-readable output."`
	JQ    string `name:"jq" help:"Filter --json output through this jq expression."`
}

// --- Column --------------------------------------------------------

// ColumnCmd groups the column verbs. Verb bodies live in column.go.
type ColumnCmd struct {
	Add  ColumnAddCmd  `cmd:"" help:"Add a column to a table."`
	List ColumnListCmd `cmd:"" help:"List columns on a table."`
}

// ColumnAddCmd wires `moltable column add --table <id> --name <n> --source <type>`
// plus exactly one of the three config-source flags. The config payload
// is non-trivial JSON (especially for Moltygent columns), so agents
// almost always pipe via stdin; humans get --config-file/--config for
// the rare one-liner case. Body in column.go.
type ColumnAddCmd struct {
	Table       string `name:"table" required:"" help:"Table ID to add the column to."`
	Name        string `name:"name" required:"" help:"Column name."`
	Source      string `name:"source" required:"" help:"Source type: input | formula | http | js | ai | webhook | send_to_table | integration | moltygent."`
	ConfigStdin bool   `name:"config-stdin" help:"Read source_config JSON from stdin (agent-friendly)."`
	ConfigFile  string `name:"config-file" help:"Read source_config JSON from this path." type:"existingfile"`
	ConfigArg   string `name:"config-arg" help:"Inline source_config JSON. Use for one-liners; prefer --config-stdin or --config-file for non-trivial configs."`
	JSON        bool   `name:"json" help:"Emit JSON instead of human-readable output."`
	JQ          string `name:"jq" help:"Filter --json output through this jq expression."`
}

// ColumnListCmd wires `moltable column list --table <id>`. Body in column.go.
type ColumnListCmd struct {
	Table string `name:"table" required:"" help:"Table ID whose columns to list."`
	JSON  bool   `name:"json" help:"Emit JSON instead of human-readable output."`
	JQ    string `name:"jq" help:"Filter --json output through this jq expression."`
}

// --- Row -----------------------------------------------------------

// RowCmd groups the row verbs. Verb bodies live in row.go.
type RowCmd struct {
	Create RowCreateCmd `cmd:"" help:"Create a single row."`
	Import RowImportCmd `cmd:"" help:"Bulk-import rows from CSV."`
}

// RowCreateCmd wires `moltable row create --table <id> --data '{...}'`.
// `--data` carries the row's input-value JSON object (column name → value).
// Names: we keep `--json` strictly for output-format toggling (matches
// the rest of the CLI); the payload flag is `--data` so there's no
// collision. Body in row.go.
type RowCreateCmd struct {
	Table string `name:"table" required:"" help:"Table ID to create the row in."`
	Data  string `name:"data" required:"" help:"Row input values as a JSON object, e.g. '{\"Name\":\"Au Cheval\"}'."`
	JSON  bool   `name:"json" help:"Emit JSON instead of human-readable output."`
	JQ    string `name:"jq" help:"Filter --json output through this jq expression."`
}

// RowImportCmd wires `moltable row import --table <id> --csv path`.
// The CSV header row must match the table's column names (case-sensitive)
// unless `--column-mapping` overrides specific entries with `col-name=csv-column`.
// Bulk POST endpoint doesn't exist today (verified against router.go);
// we stream individual POST /v1/tables/{id}/rows calls and report a
// final {imported, skipped, errors} summary. Body in row.go.
type RowImportCmd struct {
	Table         string   `name:"table" required:"" help:"Table ID to import rows into."`
	CSV           string   `name:"csv" required:"" help:"Path to the CSV file to import." type:"existingfile"`
	ColumnMapping []string `name:"column-mapping" help:"Remap CSV columns: col-name=csv-column (repeatable)."`
	JSON          bool     `name:"json" help:"Emit a JSON summary instead of progress dots + human line."`
	JQ            string   `name:"jq" help:"Filter --json output through this jq expression."`
}

// --- Run / Watch / Stop --------------------------------------------

// RunCmd groups the execution verbs. Verb bodies live in run.go.
type RunCmd struct {
	Table RunTableCmd `cmd:"" help:"Run all enrichments on a table."`
	Cell  RunCellCmd  `cmd:"" help:"Run a single column on a single row."`
	Watch RunWatchCmd `cmd:"" help:"Stream live updates for an in-flight run."`
}

// RunTableCmd wires `moltable run table <id> [--watch] [--wait]
// [--timeout <duration>]`. POSTs to /v1/tables/{id}/execute/table and
// optionally watches the resulting job via SSE (--watch) or polls until
// terminal (--wait). Body in run.go.
type RunTableCmd struct {
	ID      string        `arg:"" name:"id" help:"Table ID to run."`
	Watch   bool          `name:"watch" help:"Stream live progress events from the SSE stream until the job terminates."`
	Wait    bool          `name:"wait" help:"Poll the job until it terminates (CI-friendly alternative to --watch)."`
	Timeout time.Duration `name:"timeout" default:"1h" help:"Max wall time for --watch / --wait (e.g. 30s, 5m, 1h)."`
	JSON    bool          `name:"json" help:"Emit JSON instead of human-readable output (and JSON-lines under --watch)."`
	JQ      string        `name:"jq" help:"Filter --json output through this jq expression."`
}

// RunCellCmd wires `moltable run cell --table <id> --row <id>
// --column <id> [--watch] [--wait] [--timeout <duration>]`. POSTs to
// /v1/tables/{id}/execute/cell with {row_id, column_id}. Body in run.go.
type RunCellCmd struct {
	Table   string        `name:"table" required:"" help:"Table ID."`
	Row     string        `name:"row" required:"" help:"Row ID."`
	Column  string        `name:"column" required:"" help:"Column ID."`
	Watch   bool          `name:"watch" help:"Stream live progress events from the SSE stream until the job terminates."`
	Wait    bool          `name:"wait" help:"Poll the job until it terminates."`
	Timeout time.Duration `name:"timeout" default:"1h" help:"Max wall time for --watch / --wait."`
	JSON    bool          `name:"json" help:"Emit JSON instead of human-readable output (and JSON-lines under --watch)."`
	JQ      string        `name:"jq" help:"Filter --json output through this jq expression."`
}

// watchFlags is the shared arg + flag surface used by both `moltable
// run watch <job-id>` and the top-level `moltable watch <job-id>` alias.
// Keeping a single definition means a `--timeout` or `--json` change
// can't drift between the two entry points.
type watchFlags struct {
	JobID   string        `arg:"" name:"job-id" help:"Job ID to follow."`
	Timeout time.Duration `name:"timeout" default:"1h" help:"Max wall time before giving up."`
	JSON    bool          `name:"json" help:"Emit JSON-lines events on stdout."`
	JQ      string        `name:"jq" help:"Filter --json output through this jq expression (applied per JSON-line event)."`
}

// RunWatchCmd wires `moltable run watch <job-id>`. Resolves the job
// (GET /v1/jobs/{id}) for its table ID, then opens that table's SSE
// stream and filters events to the specified job. Body in watch.go.
type RunWatchCmd struct {
	watchFlags
}

// WatchCmd is the top-level alias for `run watch` — exposed as a
// convenience because watching a running job is the most common
// follow-up after `run table` / `run cell` and the alias keeps the
// invocation short for both humans and agent harnesses. Shares all
// arguments and the implementation in watch.go.
type WatchCmd struct {
	watchFlags
}

// StopCmd wires `moltable stop <table-id>`. POSTs to
// /v1/tables/{id}/stop. Body in stop.go.
type StopCmd struct {
	ID   string `arg:"" name:"id" help:"Table ID whose running jobs should be stopped."`
	JSON bool   `name:"json" help:"Emit JSON instead of human-readable output."`
	JQ   string `name:"jq" help:"Filter --json output through this jq expression."`
}

// --- Version / Upgrade / Skills / Config ---------------------------

// VersionCmd prints the binary version + the server-floor version.
// Delegates to runVersion (in version.go); supports --json for
// agent consumers.
type VersionCmd struct {
	JSON bool   `name:"json" help:"Emit JSON: {\"binary_version\":..., \"min_server_version\":...}."`
	JQ   string `name:"jq" help:"Filter --json output through this jq expression."`
}

func (c *VersionCmd) Run(kctx *kong.Context) error {
	return runVersion(kctx, c.JSON, c.JQ)
}

// UpgradeCmd self-updates the running binary by downloading the
// release tarball from GitHub, verifying its sha256, and atomically
// swapping it via go-update. Without --version it picks the latest;
// with --version it pins to a specific tag (e.g. "0.5.0"). With
// --check-only it skips the download and just reports whether a newer
// release exists (exit 0 when newer exists, 1 when the current binary
// is already latest — keeps the background-nudge goroutine simple).
type UpgradeCmd struct {
	Version   string `name:"version" help:"Pin to a specific release tag (e.g. 0.5.0). Empty fetches the latest."`
	CheckOnly bool   `name:"check-only" help:"Only print whether a newer release exists; do not download or swap."`
	JSON      bool   `name:"json" help:"Emit machine-readable JSON output (check-only mode)."`
	JQ        string `name:"jq" help:"Filter --json output through this jq expression."`
}

func (c *UpgradeCmd) Run(kctx *kong.Context) error {
	return runUpgrade(kctx, c.Version, c.CheckOnly, c.JSON, c.JQ)
}

// SkillsCmd, SkillsInstallCmd, and SkillsUninstallCmd live in
// skills.go alongside the install logic + embed wrapper.

// ConfigCmd exposes the resolved config path and current contents,
// plus the get/set verbs needed by the long-tail fallback skill.
// `get api-key` is the critical entry point — agents call it to learn
// the active API key before issuing a downstream HTTP call.
type ConfigCmd struct {
	Path ConfigPathCmd `cmd:"" help:"Print the resolved config file path."`
	Show ConfigShowCmd `cmd:"" help:"Print the active config (redacts API keys)."`
	Get  ConfigGetCmd  `cmd:"" help:"Print one config value (api-key | profile | default-profile)."`
	Set  ConfigSetCmd  `cmd:"" help:"Set one config value. Today only default-profile is writeable."`
}

type ConfigPathCmd struct{}

func (c *ConfigPathCmd) Run(kctx *kong.Context, root *CLI) error {
	if root.Config != "" {
		fmt.Fprintln(kctx.Stdout, root.Config)
		return nil
	}
	p, err := config.Path()
	if err != nil {
		return err
	}
	fmt.Fprintln(kctx.Stdout, p)
	return nil
}

type ConfigShowCmd struct {
	JSON bool   `name:"json" help:"Emit JSON (api keys are sanitized to molt_xxxxxxxx)."`
	JQ   string `name:"jq" help:"Filter --json output through this jq expression."`
}

func (c *ConfigShowCmd) Run(kctx *kong.Context, root *CLI) error {
	return runConfigShow(kctx, root, c.JSON, c.JQ)
}

// ConfigGetCmd prints one value. The api-key key returns the
// PLAINTEXT resolved key — agents need it to make downstream calls.
type ConfigGetCmd struct {
	Key string `arg:"" required:"" help:"Which value to print: api-key | profile | default-profile."`
}

func (c *ConfigGetCmd) Run(kctx *kong.Context, root *CLI) error {
	return runConfigGet(kctx, root, c.Key)
}

// ConfigSetCmd writes a value to the config and saves. Today only the
// `default-profile` key is supported; other keys return an
// InvalidInputError directing the user at the right command.
type ConfigSetCmd struct {
	Key   string `arg:"" required:"" help:"Which value to set. Today only default-profile."`
	Value string `arg:"" required:"" help:"The new value."`
}

func (c *ConfigSetCmd) Run(kctx *kong.Context, root *CLI) error {
	return runConfigSet(kctx, root, c.Key, c.Value)
}

// --- helpers -------------------------------------------------------

// loadConfig reads from the override path if --config was passed,
// else from the XDG-resolved default location.
func loadConfig(override string) (*config.Config, error) {
	if override != "" {
		return config.LoadFrom(override)
	}
	return config.Load()
}

// run is split out of main so tests can drive it without os.Exit.
func run(args []string, stdout, stderr *os.File) int {
	cli := &CLI{}
	parser, err := kong.New(cli,
		kong.Name("moltable"),
		kong.Description("moltable CLI — drive the moltable API from the shell."),
		kong.UsageOnError(),
		kong.Writers(stdout, stderr),
		kong.Vars{"version": version},
		// Group titles for the gh-style help renderer. Keys match
		// the `group:"…"` tags on the CLI struct's command fields;
		// titles render as ALL-CAPS section headers.
		kong.Groups{
			"core":       "Core Commands",
			"alias":      "Alias Commands",
			"additional": "Additional Commands",
		},
		// Replace Kong's flat command listing with the grouped
		// gh-style layout for the root. Subtree help falls through
		// to kong.DefaultHelpPrinter — see ui.RenderHelp.
		kong.Help(ui.RenderHelp),
	)
	if err != nil {
		fmt.Fprintf(stderr, "%s %v\n", ui.Error("moltable:"), err)
		return 1
	}
	kctx, err := parser.Parse(args)
	if err != nil {
		// DX: Kong returns *kong.ParseError "expected one of ..." when
		// the user types bare `moltable` or `moltable <noun>` with no
		// verb — both are exploration, not errors. Show context-aware
		// help on stdout and exit 0 (matches gh's bare-command UX).
		// Other parse errors (unknown flag, bad arg type) keep the
		// usual exit 1 + stderr message path.
		if expectingSubcommand(err) {
			// Special case: `moltable auth` should DO something, not
			// just show help — the user is telling us they want to
			// engage with auth. Pick the most useful verb based on
			// current state: no profile → start login; profile present
			// → show check status. Other nouns fall through to scoped
			// help (they're CRUD verbs where guessing is risky).
			if usingNounAuth(args) {
				return runAuthDefault(parser, cli, stdout, stderr, args)
			}
			printIntroAfterMissingCommand(stdout, stderr, parser, cli, args)
			return 0
		}
		// `unexpected argument <tok>` → unknown command. Render the
		// gh-style screen (header + "Did you mean this?" + Usage +
		// Available commands) instead of Kong's one-liner. Done at
		// the scope where the bad token landed: bare unknown uses
		// root children, `moltable auth lgin` uses auth's children.
		if tok, scope, ok := unknownCommand(err); ok {
			ui.UnknownCommand(stderr,
				cliPath("moltable", scope),
				tok,
				ui.Suggest(tok, nodeChildNames(scope), 2),
				nodeChildNames(scope),
			)
			return 1
		}
		// Kong's UsageOnError doesn't fire with the manual parser.Parse
		// path — surface the error sentence ourselves so the user isn't
		// staring at a silent exit, and append a hint so unknown
		// commands / flags don't end at a dead-end.
		fmt.Fprintf(stderr, "%s %v\n", ui.Error("moltable:"), err)
		fmt.Fprintln(stderr, "hint: Run `moltable --help` to see available commands.")
		return 1
	}

	// Background 24h update-check nudge. Fired before the command body
	// runs so any latency overlaps the command's own work. The helper
	// internally honors MOLTABLE_NO_UPDATE_CHECK, checks whether
	// stderr is a TTY, and recognizes --json so it stays silent when
	// piped or in agent mode. The cancel callback is invoked at the
	// end of run() to give the goroutine a brief grace window to
	// print its nudge before the process exits.
	cancelCheck := startBackgroundUpdateCheck(stderr, kctx, args)

	if err := kctx.Run(cli); err != nil {
		// `upgrade --check-only` returns errCurrent (silent sentinel)
		// when the binary is already latest — exit 1 with NO stderr
		// noise so the background goroutine treats it as "no nudge".
		if IsAlreadyCurrent(err) {
			cancelCheck()
			return 1
		}
		// Print the user-facing message + (when available) the hint.
		// errors.As walks the chain so hints survive any wrapping
		// Kong might add via reflection-based dispatch.
		fmt.Fprintf(stderr, "%s %v\n", ui.Error("moltable:"), err)
		var h interface{ Hint() string }
		if errors.As(err, &h) {
			fmt.Fprintf(stderr, "%s %s\n", ui.Hint("hint:"), ui.Hint(h.Hint()))
		}
		cancelCheck()
		if errors.Is(err, auth.ErrNoAuth) {
			return 2 // exit code 2 is reserved for auth errors.
		}
		// Map typed errors (NotFoundError, RateLimitError, etc.) to
		// their dedicated exit codes. Anything not in the table falls
		// through to ExitGeneric == 1.
		return clierrors.ExitCode(err)
	}
	cancelCheck()
	return 0
}

// startBackgroundUpdateCheck spawns a fire-and-forget goroutine that
// asks the updater for the latest release version and prints a stderr
// nudge if there's a newer one. Returns a `wait` function the caller
// invokes right before returning from run() — it gives the goroutine
// a brief moment to either finish or be skipped.
//
// Suppression rules (any → no nudge):
//
//   - MOLTABLE_NO_UPDATE_CHECK=1 in env
//   - `--json` appeared anywhere in the parsed args (agent-driven)
//   - stderr is NOT a terminal (piped to file or another process)
//
// The check itself is 100% cache-aware — fresh cache (<24h) returns
// instantly without a network call. The first call after the cache
// expires pays the GitHub round-trip; we cap that at 2 seconds so a
// slow GitHub can't delay command exit.
func startBackgroundUpdateCheck(stderr *os.File, kctx *kong.Context, args []string) func() {
	noop := func() {}
	if os.Getenv("MOLTABLE_NO_UPDATE_CHECK") == "1" {
		return noop
	}
	// `--json` on any verb → caller is an agent or script; no nudge.
	for _, a := range args {
		if a == "--json" {
			return noop
		}
	}
	// Stderr not a TTY → presumed redirected/piped; no nudge.
	if info, err := stderr.Stat(); err == nil {
		if info.Mode()&os.ModeCharDevice == 0 {
			return noop
		}
	}
	// Skip self-check when the command IS upgrade — no point telling
	// the user "an upgrade is available" while they're upgrading.
	if kctx != nil && kctx.Command() == "upgrade" {
		return noop
	}

	done := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		client := updater.NewClient()
		res, err := client.CheckLatest(ctx, clversion.BinaryVersion)
		if err != nil || !res.HasUpdate {
			return
		}
		fmt.Fprintf(stderr,
			"\nA new release of moltable is available: %s -> %s\nRun `moltable upgrade` to update.\n",
			clversion.BinaryVersion, res.Latest,
		)
	}()
	// `wait` is what run() calls right before returning. We give the
	// goroutine up to 100ms after the command finishes so a fast
	// cache hit can print its nudge; if it isn't ready by then we
	// abandon it (process exits cleanly).
	return func() {
		once.Do(func() {
			select {
			case <-done:
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

// expectingSubcommand returns true when Kong's ParseError is the
// "expected one of <verbs>" shape — i.e. the user reached a node in
// the command tree that requires a sub-command but didn't supply one.
// This is exploration, not a typo, so the caller switches to the
// friendly help path instead of exiting with an error.
func expectingSubcommand(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "expected one of")
}

// unknownCommand recognizes Kong's "unexpected argument <tok>" parse
// error, recovers the bad token and the *scope* it landed in (the
// node Kong successfully descended into before failing), and returns
// both. ok == false for any other error shape.
//
// Why scope matters: `moltable lgin` should suggest from root
// children (auth, workbook, table, …) but `moltable auth lgin` should
// suggest from auth's children (login, logout, check). Kong gives
// us the right scope via ParseError.Context.Selected().
func unknownCommand(err error) (token string, scope *kong.Node, ok bool) {
	if err == nil {
		return "", nil, false
	}
	const marker = "unexpected argument "
	msg := err.Error()
	if !strings.HasPrefix(msg, marker) {
		return "", nil, false
	}
	rest := msg[len(marker):]
	// Strip Kong's helpful suffix ", did you mean ..." so we only
	// keep the typed token. We'll regenerate suggestions ourselves.
	if i := strings.Index(rest, ","); i >= 0 {
		rest = rest[:i]
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", nil, false
	}
	// Recover the scope from the ParseError. Selected() is the
	// deepest node Kong matched before the failure. nil means we
	// failed at the root, so use the model's root node.
	var pe *kong.ParseError
	if errors.As(err, &pe) && pe.Context != nil {
		sc := pe.Context.Selected()
		if sc == nil {
			sc = pe.Context.Model.Node
		}
		return rest, sc, true
	}
	return rest, nil, true
}

// cliPath formats the scope path for the gh-style "for X" header
// (e.g. `moltable`, `moltable auth`). Walks parents up to (but not
// including) the model's synthetic root.
func cliPath(cliName string, n *kong.Node) string {
	if n == nil {
		return cliName
	}
	// Build child→root list, then reverse + join.
	var parts []string
	for cur := n; cur != nil && cur.Name != "" && cur.Type != kong.ApplicationNode; cur = cur.Parent {
		parts = append([]string{cur.Name}, parts...)
	}
	if len(parts) == 0 {
		return cliName
	}
	return cliName + " " + strings.Join(parts, " ")
}

// nodeChildNames returns the visible child command names for a node,
// alphabetized. Hidden children are filtered out so internal/debug
// commands stay out of suggestion lists.
func nodeChildNames(n *kong.Node) []string {
	if n == nil {
		return nil
	}
	var names []string
	for _, c := range n.Children {
		if c.Hidden {
			continue
		}
		names = append(names, c.Name)
	}
	sort.Strings(names)
	return names
}

// printIntroAfterMissingCommand renders the friendly help that fires
// when bare `moltable` or `moltable <noun>` is invoked. The Kong parse
// already failed (no kctx was returned), so we re-parse with --help
// appended to lean on Kong's own usage renderer for the matched
// subtree, then prepend a short banner + an auth-status line for the
// bare-root case so users see live state, not just a help wall.
func printIntroAfterMissingCommand(stdout, stderr *os.File, parser *kong.Kong, cli *CLI, args []string) {
	// Bare `moltable` — show banner + auth status before the grouped
	// help renderer runs. Colors gated by ui (no-op when !TTY) so
	// `moltable | cat` stays clean text.
	if len(args) == 0 || allDashFlags(args) {
		fmt.Fprintf(stdout, "%s %s — drive the moltable API from the shell.\n\n",
			ui.Bold("moltable"), ui.Accent(clversion.BinaryVersion))

		cfg, _ := loadConfig(cli.Config)
		in := auth.FromEnvironment(cli.APIKey, cfg)
		if cli.Profile != "" && in.FlagAPIKey == "" && in.EnvAPIKey == "" {
			in.EnvProfile = cli.Profile
		}
		_, src, rerr := auth.Resolve(in)
		base := resolveAPIBase(cli.Dev)
		switch {
		case rerr != nil:
			fmt.Fprintf(stdout, "%s not configured (target: %s)\n", ui.Warn("Auth:"), ui.Accent(base))
			fmt.Fprintf(stdout, "      Run %s to get started.\n", ui.Bold("`moltable auth login`"))
		default:
			who := "configured"
			if src != "" {
				who = string(src)
			}
			fmt.Fprintf(stdout, "%s %s (target: %s)\n",
				ui.Success("Auth:"), ui.Bold(who), ui.Accent(base))
		}
		fmt.Fprintln(stdout, "")
	}

	// Lean on Kong: re-parse the same args with --help appended so the
	// usage renderer shows the matched subtree (root or noun). Kong's
	// default behavior when --help is detected is to write help AND
	// call os.Exit(0) — which would terminate the in-process test
	// harness (runCLI) before run() returns. Build a throwaway parser
	// with kong.Exit overridden so the test harness can exercise this
	// path without the host process dying.
	helpParser, err := kong.New(cli,
		kong.Name("moltable"),
		kong.Description("moltable CLI — drive the moltable API from the shell."),
		kong.Writers(stdout, stderr),
		kong.Vars{"version": version},
		kong.Exit(func(int) {}),
		// Must mirror the main parser's group + help options so the
		// banner + help output stays consistent whether the user
		// invoked `moltable --help` (main parser) or bare `moltable`
		// (this re-parse path).
		kong.Groups{
			"core":       "Core Commands",
			"alias":      "Alias Commands",
			"additional": "Additional Commands",
		},
		kong.Help(ui.RenderHelp),
	)
	if err != nil {
		return
	}
	helpArgs := append([]string{}, args...)
	helpArgs = append(helpArgs, "--help")
	_, _ = helpParser.Parse(helpArgs)
}

// allDashFlags reports whether every arg is a `-x` / `--xxx` flag —
// used to detect "bare invocation with only global flags" (e.g.
// `moltable --dev`) as still bare-root for the help banner.
func allDashFlags(args []string) bool {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return false
		}
	}
	return true
}

// valueTakingGlobalFlags lists the CLI's space-separated value-taking
// global flags. `--api-key molt_x` parses as TWO tokens (flag + value)
// where the second token MUST be skipped during noun detection — otherwise
// `moltable --profile auth workbook list` would have `auth` mis-classified
// as the standalone noun and trigger the smart default. The `=` form
// (`--profile=auth`) is a single token and needs no special handling.
//
// Keep this in sync with the CLI struct's `name:"…"` tags on value-taking
// global flags. `--dev` is bool-typed and intentionally NOT listed.
var valueTakingGlobalFlags = map[string]bool{
	"--api-key": true,
	"--profile": true,
	"--config":  true,
}

// usingNounAuth reports whether the args boil down to `moltable auth`
// (plus any global flags) — i.e. the user typed the noun without a
// verb. Correctly skips the value of space-separated value-taking
// flags so `moltable --profile auth workbook list` (auth-as-value)
// does NOT trigger the smart default.
func usingNounAuth(args []string) bool {
	saw := false
	i := 0
	for i < len(args) {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			// Space-separated `--profile auth`: the next token is the
			// flag value, not a noun. Skip both.
			if valueTakingGlobalFlags[a] && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i += 2
				continue
			}
			i++
			continue
		}
		if saw {
			return false // a second non-flag token → not bare `auth`
		}
		if a != "auth" {
			return false
		}
		saw = true
		i++
	}
	return saw
}

// runAuthDefault is what fires when the user types `moltable auth`
// with no verb. We pick:
//   - no profile resolves   → run `auth login`  (start the dance)
//   - profile resolves      → run `auth check`  (show what they have)
//
// Both are reachable by typing `moltable auth login` / `auth check`
// explicitly; this just removes the "memorize the verb" step for the
// most common entry point.
func runAuthDefault(parser *kong.Kong, cli *CLI, stdout, stderr *os.File, args []string) int {
	cfg, _ := loadConfig(cli.Config)
	in := auth.FromEnvironment(cli.APIKey, cfg)
	if cli.Profile != "" && in.FlagAPIKey == "" && in.EnvAPIKey == "" {
		in.EnvProfile = cli.Profile
	}
	_, _, rerr := auth.Resolve(in)

	verb := "check"
	if rerr != nil {
		verb = "login"
	}

	// Smart-default refuses to start the browser-handoff dance when the
	// caller isn't interactive. `moltable auth` in a CI script, a piped
	// pre-commit hook, or any context where stderr isn't a TTY would
	// otherwise block the poll loop for 5 minutes AND pop a browser tab
	// nobody can see. Fail fast with an actionable hint instead.
	// `auth check` is safe to run unattended, so the gate only fires
	// on the login branch.
	if verb == "login" && !isTTY(stderr) {
		fmt.Fprintln(stderr, "moltable: `moltable auth` would run `auth login` (no profile resolves), but stderr is not a terminal.")
		fmt.Fprintln(stderr, "hint: For automation, set MOLTABLE_API_KEY or configure a profile. For interactive login, run `moltable auth login` in a terminal.")
		return 2
	}

	// Strip the standalone `auth` token and append the chosen verb,
	// preserving any global flags the user already supplied. Re-parse
	// + run via the same parser so global state (--dev, --profile,
	// --api-key) flows through normally.
	rewritten := make([]string, 0, len(args)+1)
	consumed := false
	for _, a := range args {
		if !consumed && !strings.HasPrefix(a, "-") && a == "auth" {
			rewritten = append(rewritten, "auth", verb)
			consumed = true
			continue
		}
		rewritten = append(rewritten, a)
	}

	// Tell the user what we picked so the "magic default" isn't
	// invisible. Routed to stderr (--json future-proofing) and styled
	// as a status line (`->`) rather than an error so it doesn't
	// collide visually with the `moltable: ...` error prefix.
	fmt.Fprintf(stderr, "-> running `moltable auth %s` (default for bare `auth`).\n", verb)

	kctx2, err := parser.Parse(rewritten)
	if err != nil {
		fmt.Fprintf(stderr, "%s %v\n", ui.Error("moltable:"), err)
		return 1
	}
	// Mirror run()'s background update-check on the smart-default path
	// so the most common entry point isn't permanently nudge-suppressed.
	cancelCheck := startBackgroundUpdateCheck(stderr, kctx2, rewritten)
	defer cancelCheck()
	if err := kctx2.Run(cli); err != nil {
		fmt.Fprintf(stderr, "%s %v\n", ui.Error("moltable:"), err)
		var h interface{ Hint() string }
		if errors.As(err, &h) {
			fmt.Fprintf(stderr, "%s %s\n", ui.Hint("hint:"), ui.Hint(h.Hint()))
		}
		if errors.Is(err, auth.ErrNoAuth) {
			return 2
		}
		return clierrors.ExitCode(err)
	}
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
