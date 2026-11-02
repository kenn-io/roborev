package main

// Entries add concepts and prerequisites that are absent from existing command
// descriptions. Commands with substantial descriptions retain their own text.
var roborevCommandHelp = map[string]struct{ long, example string }{
	"review": {long: `Enqueue a background code review for HEAD, another commit, a range, or
uncommitted changes. The daemon records a job ID before running the configured
agent. Use wait and show with that ID to retrieve the result; --local instead
runs without the daemon. --wait exits 1 when the completed review fails.`},
	"status": {long: `Inspect the daemon, queue, browser UI and Agent Hook reminders. This is
the same status view as daemon status; use doctor for setup diagnosis.`, example: `  roborev status
  roborev status --json`},
	"close": {long: `Mark a review resolved after addressing or dismissing its findings. The
argument is a job ID, not a git ref. Closing preserves its output and comments;
--reopen returns it to the open ledger.`, example: `  roborev close 42
  roborev close 42 --reopen`},
	"cancel": {example: `  roborev cancel 42`},
	"search": {long: `Search completed review history by words or meaning. Use --lexical for
keyword matches, --semantic for meaning, or --hybrid for both. Filters narrow
repository, branch, verdict and time. --agent prints concise result rows;
--json returns the daemon response. Neither creates a new review.`, example: `  roborev search 'shutdown race' --agent
  roborev search 'retry failures' --lexical --verdict fail --json`},
	"init": {example: `  roborev init
  roborev init --no-daemon`},
	"init gh-action": {example: `  roborev init gh-action
  roborev init gh-action --output .github/workflows/review.yml`},
	"quickstart": {example: `  roborev quickstart
  roborev quickstart --json`},
	"signing-init": {long: `Create a new private signing key file for read-only remote HTTPS access.
Keep the file private and configure its key ID and repository grants on the
server separately; creating a key does not enable a remote listener.`, example: `  roborev signing-init --secret-file ./reader.key`},
	"snooze": {example: `  roborev snooze on
  roborev snooze off`},
	"pause": {long: `Pause the daemon's review queue. Queued jobs remain stored and can resume
with unpause. Use cancel to stop one queued or running job instead.`, example: `  roborev pause`},
	"unpause": {long: `Resume queue processing after pause. The daemon continues stored queued
jobs using its configured workers; this does not create new review jobs.`, example: `  roborev unpause`},
	"install-hook": {long: `Install roborev's managed git hooks in the current repository so commits
queue reviews automatically. Existing user hook content is preserved unless
you explicitly replace it with --force. Use init for the complete setup.`, example: `  roborev install-hook
  roborev install-hook --force`},
	"install-hook repair": {long: `Repair roborev-managed hook registrations after executable paths or git
hook configuration change. This works on existing managed hooks; install-hook
creates registrations in a repository that has none.`, example: `  roborev install-hook repair`},
	"uninstall-hook": {long: `Remove roborev's managed post-commit, post-rewrite and pre-push hooks from
the current repository. Existing review records remain in the daemon.`, example: `  roborev uninstall-hook`},
	"post-commit": {long: `Git calls this entry point after a commit to enqueue its review according
to repository policy. Normally install-hook registers it for you; use review
when you want to choose a commit explicitly.`, example: `  roborev post-commit`},
	"daemon": {long: `Manage the local background process that owns review storage and runs
queued jobs. Most commands discover and start it automatically. Explicit
--server targets are connections to an existing daemon.`, example: `  roborev daemon status
  roborev daemon start`},
	"daemon start": {long: `Start the local daemon in the background if necessary. It opens the
configured data directory and resumes queued review work. Use daemon status
to inspect the queue and published browser address.`, example: `  roborev daemon start`},
	"daemon stop": {long: `Stop the local daemon. Stored review history and queued jobs remain
available when it starts again. Use pause to suspend queue processing while
leaving the daemon available for reads.`, example: `  roborev daemon stop`},
	"daemon restart": {long: `Stop and start the local daemon to load new configuration or a new binary.
If no daemon is running, start it. Review history stays in the data directory.`, example: `  roborev daemon restart`},
	"daemon status": {long: `Inspect the local daemon, its queue, browser UI and Agent Hook reminders.
Use --json for the status record, or doctor to diagnose setup and agents.`, example: `  roborev daemon status
  roborev daemon status --json`},
	"daemon run": {example: `  roborev daemon run`},
	"tui": {example: `  roborev tui
  roborev tui --repo --branch`},
	"ui": {long: `Open the daemon's browser review workspace. An optional numeric job ID
opens that review directly. The daemon must have embedded web assets and an
enabled browser listener; status reports the published address.`, example: `  roborev ui
  roborev ui 42`},
	"mcp": {example: `  roborev mcp serve
  roborev mcp status`},
	"mcp serve":  {example: `  roborev mcp serve`},
	"mcp status": {example: `  roborev mcp status --json`},
	"mcp install": {long: `Register roborev's MCP server in coding-agent configuration. The default
stdio transport starts an installed executable; HTTP connects to an existing
daemon MCP endpoint. --dry-run prints the merged config without writing it.`, example: `  roborev mcp install --agent codex --dry-run`},
	"refine": {example: `  roborev refine --list
  roborev refine --max-iterations 5`},
	"export": {long: `Export stored review content or CI metrics as JSON for downstream tools.
Subcommands define their own filters and resumable cursors. Persist each
returned next_cursor only after consuming its data.`, example: `  roborev export reviews
  roborev export ci-metrics`},
	"export reviews": {example: `  roborev export reviews
  roborev export reviews --profile metadata --since 2026-01-01`},
	"export ci-metrics": {example: `  roborev export ci-metrics --since 2026-01-01`},
	"export ci-costs":   {example: `  roborev export ci-costs --since 2026-01-01`},
	"repo": {example: `  roborev repo list
  roborev repo show example`},
	"repo list": {example: `  roborev repo list`},
	"skills": {long: `Install reusable roborev instructions into coding-agent configuration.
Install adds skills for selected or detected agents; update refreshes only
agents that already have them. These commands do not start a review.`, example: `  roborev skills install
  roborev skills update`},
	"skills install": {example: `  roborev skills install`},
	"skills update":  {example: `  roborev skills update`},
	"sync": {long: `Manage optional PostgreSQL synchronization of local review history.
Configure sync before using these commands; the local daemon owns the sync
worker and its status.`, example: `  roborev sync status`},
	"sync status": {long: `Inspect whether PostgreSQL sync is enabled and the daemon's latest sync
state. This reads status without triggering an extra cycle; use sync now
when you need an immediate attempt.`, example: `  roborev sync status`},
	"sync now": {example: `  roborev sync now`},
	"remap":    {example: `  roborev remap`},
	"agent-hook": {long: `Install optional coding-agent harness callbacks and inspect their tracked
sessions. These reminders bring open findings back into an agent session;
the git post-commit hook separately creates background reviews.`, example: `  roborev agent-hook install --dry-run
  roborev agent-hook status`},
	"agent-hook install": {long: `Register optional harness hooks for detected or selected coding agents.
Installation also updates bundled skills. Add review_guidelines or REVIEW.md
before relying on reminders. --dry-run shows configuration changes first.`, example: `  roborev agent-hook install --dry-run
  roborev agent-hook install --agent codex`},
	"agent-hook dump": {long: `Print an agent's native hook configuration for inspection or manual
installation. Select one profile with --agent; this prints configuration
without registering it in the agent's files.`, example: `  roborev agent-hook dump --agent codex`},
	"agent-hook status": {long: `Print tracked Agent Hook session counters as JSON. These counters drive
reminders and are separate from the daemon's stored review jobs.`, example: `  roborev agent-hook status`},
	"agent-hook reset": {long: `Reset reminder counters for one harness session, or all sessions with
--all. This does not delete reviews or close their findings.`, example: `  roborev agent-hook reset example-session
  roborev agent-hook reset --all`},
	"agent-hook run": {long: `Handle a harness callback payload on stdin and emit the native hook
response on stdout. agent-hook install supplies the registration arguments;
manual users should use install or dump rather than construct callbacks.`, example: `  roborev agent-hook run --agent codex --source roborev-agent-hook`},
	"agent-hook fix-done": {long: `Complete the Agent Hook fix session identified by its UUID after finishing
its requested work. This releases the session's reminder state; it does not
replace review comments or close arbitrary jobs.`, example: `  roborev agent-hook fix-done 11111111-1111-4111-8111-111111111111`},
	"ci": {long: `Run review matrices inside CI using explicit flags or forge-provided
environment. CI reviews can run agents and post pull-request comments when
requested; they are separate from local git-hook queueing.`, example: `  roborev ci review --ref HEAD~1..HEAD`},
	"ci review": {example: `  roborev ci review --ref HEAD~1..HEAD
  roborev ci review --gh-repo example/project --pr 42 --comment`},
	"config": {long: `Read and change global or repository configuration. Reads merge repository
values over global defaults unless --global or --local selects one scope.
Writes default to the repository file; use --global for personal defaults.`, example: `  roborev config list --show-origin
  roborev config validate`},
	"config get": {long: `Read one configuration key from the merged configuration. Use --global or
--local to inspect only that file, and config list to discover known values.`, example: `  roborev config get default_agent --global`},
	"config set": {long: `Write a configuration value to the repository file by default, or the
global file with --global. Keys and value types are validated; use config
validate to check the resulting configuration. Common global keys include
 default_agent, default_model and review_reasoning; repository keys include
 agent, model and review_guidelines. config list shows explicitly configured
 values, rather than a catalogue of every supported key.`, example: `  roborev config set agent codex --local
  roborev config set default_agent codex --global`},
	"config list": {long: `List configuration values from the selected scope. Without a scope flag,
repository settings override global settings. --show-origin identifies
the file supplying each value.`, example: `  roborev config list --show-origin`},
	"config validate": {long: `Check configuration keys, values and experiments without modifying them.
The default combines repository and global settings; --local and --global
limit validation to one scope.`, example: `  roborev config validate
  roborev config validate --global`},
	"legacy-reviews": {long: `Recover historical Markdown reviews into canonical JSON documents. First
try deterministic conversion; export unresolved records for manual or agent
conversion and import validated documents afterward.`, example: `  roborev legacy-reviews convert
  roborev legacy-reviews export`},
	"legacy-reviews convert": {example: `  roborev legacy-reviews convert`},
	"legacy-reviews export": {long: `Export unresolved historical reviews and their JSON schema for conversion.
This preserves records that deterministic conversion could not recover;
import a validated JSON document to restore one review.`, example: `  roborev legacy-reviews export`},
	"legacy-reviews import": {long: `Read a converted canonical review document from stdin, validate it, and
restore the record identified by its archive ID (numeric for SQLite, or a UUID
with --postgres-url). Export provides unresolved records and the schema.`, example: `  roborev legacy-reviews import 42`},
	"backfill-tokens": {example: `  roborev backfill-tokens --dry-run`},
	"update":          {example: `  roborev update --check`},
	"version": {long: `Print the CLI build version and whether production browser assets are
embedded. --json returns stable machine-readable build information without
requiring a repository or running daemon.`, example: `  roborev version
  roborev version --json`},
	"help": {long: `Read the built-in workflow or one command's flags and examples. Nested
command names select a specific help topic; no daemon connection is needed.`, example: `  roborev help
  roborev help show
  roborev help config set`},
}
