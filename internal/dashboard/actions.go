package dashboard

// Action は TUI で確定した既存 CLI 操作である。
// WorkDir は起動・貸出・bench が対象にする明示的な作業元で、process の cwd は変更しない。
type Action struct {
	Args    []string
	WorkDir string
}

type menuItem struct {
	label           string
	description     string
	impact          string
	command         string
	defaultArgs     []string
	inputLabel      string
	inputNeeded     bool
	workDir         bool
	targetWorkspace bool
	targetAll       bool
	targetInput     bool
	argumentChoices []choice
	destructive     bool
	external        bool
}

var tabNames = []string{"Status", "Launch", "Settings", "Doctor", "Maintenance", "System"}

var tabMenus = map[int][]menuItem{
	1: {
		{label: "Launch Claude", description: "Launch Claude in a worktree for the selected workspace.", impact: "Uses the same worktree policy and readiness rules as the CLI.", command: "claude", inputLabel: "Claude arguments", workDir: true, argumentChoices: defaultOrCustomArguments("Launch with default options"), external: true},
		{label: "Launch Codex", description: "Launch Codex in a worktree for the selected workspace.", impact: "Hands terminal control to Codex and releases the lease after it exits.", command: "codex", inputLabel: "Codex arguments", workDir: true, argumentChoices: defaultOrCustomArguments("Launch with default options"), external: true},
		{label: "Resume a conversation", description: "Restore saved work for a wx session ID.", impact: "Does not silently fall back to a fresh worktree if restoration fails.", command: "resume", inputLabel: "wx session ID", inputNeeded: true, external: true},
		{label: "Open a shell", description: "Open a shell in a leased worktree for the selected workspace.", impact: "Saves unfinished work and releases the lease when the shell exits.", command: "shell", inputLabel: "Shell arguments", workDir: true, argumentChoices: defaultOrCustomArguments("Open with default options"), external: true},
		{label: "Run a command", description: "Run one command and its arguments in a leased worktree.", impact: "Passes the executable and arguments as separate values.", command: "run", inputLabel: "command and arguments", inputNeeded: true, workDir: true, external: true},
		{label: "Create a path lease", description: "Create a worktree lease and print its path and session ID.", impact: "The lease remains until its parent session, an explicit release, or its TTL ends it.", command: "new", inputLabel: "Lease arguments", workDir: true, argumentChoices: defaultOrCustomArguments("Use the default base")},
	},
	3: {
		{label: "Standard diagnostics", description: "Check configuration, the daemon, database, and slots.", impact: "Still reports facts available locally when the daemon is unavailable.", command: "doctor"},
		{label: "Verbose diagnostics", description: "Include passing checks and additional diagnostic detail.", impact: "Reads more information without changing managed state.", command: "doctor", defaultArgs: []string{"--verbose"}},
		{label: "Worktree probe", description: "Prepare a worktree in every registered workspace and inspect it.", impact: "Retires standby slots, so confirm before starting.", command: "doctor", defaultArgs: []string{"--probe"}},
	},
	4: {
		{label: "Garbage collection", description: "Collect managed data whose retention period has elapsed.", impact: "The daemon rechecks every candidate when the operation runs.", command: "gc", argumentChoices: dryRunChoices("Run garbage collection")},
		{label: "Clear sessions and standbys", description: "Request removal of sessions and standby slots.", impact: "Uncommitted work is discarded only when --discard is explicitly supplied.", command: "clear", inputLabel: "Clear arguments", argumentChoices: clearChoices(), destructive: true},
		{label: "Prune recovery refs", description: "Remove recovery refs that are safe to delete.", impact: "Every target is checked again when the operation runs.", command: "prune", inputLabel: "Prune arguments", argumentChoices: pruneChoices()},
		{label: "Retry standby replenishment", description: "Resume stopped standby replenishment for a workspace.", impact: "Does not modify quarantined slots.", command: "retry-standby", targetWorkspace: true, targetAll: true},
		{label: "Release a lease", description: "Explicitly release a lease created by wx new.", impact: "Normally creates a snapshot before releasing the lease.", command: "release", inputLabel: "wx session ID", inputNeeded: true, targetInput: true, argumentChoices: []choice{{label: "Save work and release"}, {label: "Discard work and release", value: "--discard"}}},
		{label: "Discard recovery state", description: "Discard quarantined recovery snapshots for a workspace.", impact: "The original working state can no longer be restored.", command: "discard-recovery", targetWorkspace: true, argumentChoices: dryRunChoices("Discard recovery state"), destructive: true},
		{label: "Forget a workspace", description: "Remove a registered workspace from wx management.", impact: "Physical removal still follows the daemon ownership rules.", command: "forget", targetWorkspace: true, destructive: true},
		{label: "Benchmark preparation", description: "Measure preparation time and storage use for a workspace.", impact: "Retires standby slots by default to measure a cold start.", command: "bench", inputLabel: "Benchmark arguments", workDir: true, argumentChoices: benchmarkChoices()},
	},
	5: {
		{label: "Start daemon", description: "Start the daemon through its LaunchAgent and wait for a response.", impact: "Distinguishes request acceptance from successful startup.", command: "daemon", defaultArgs: []string{"start"}},
		{label: "Stop daemon", description: "Ask the daemon to stop safely and wait for it to exit.", impact: "In-flight operations continue until the existing idle gate allows shutdown.", command: "daemon", defaultArgs: []string{"stop"}},
		{label: "Restart daemon", description: "Stop the daemon and wait for a new process to respond.", impact: "Also activates an updated wx binary.", command: "daemon", defaultArgs: []string{"restart"}},
	},
}

func defaultOrCustomArguments(defaultLabel string) []choice {
	return []choice{{label: defaultLabel}, {label: "Enter custom arguments…", input: true}}
}

func dryRunChoices(runLabel string) []choice {
	return []choice{{label: "Preview changes", value: "--dry-run"}, {label: runLabel}}
}

func clearChoices() []choice {
	return []choice{
		{label: "Preview default targets", value: "--dry-run"},
		{label: "Clear default targets"},
		{label: "Preview including standbys", value: "--standby --dry-run"},
		{label: "Clear including standbys", value: "--standby"},
		{label: "Enter custom arguments…", input: true},
	}
}

func pruneChoices() []choice {
	return []choice{
		{label: "Preview safe refs", value: "--dry-run"},
		{label: "Prune safe refs"},
		{label: "Preview all refs", value: "--all --dry-run"},
		{label: "Prune all refs", value: "--all"},
		{label: "Enter custom arguments…", input: true},
	}
}

func benchmarkChoices() []choice {
	return []choice{
		{label: "Run one cold preparation"},
		{label: "Run the standard configuration sweep", value: "--sweep"},
		{label: "Measure standby reuse", value: "--reuse"},
		{label: "Enter custom arguments…", input: true},
	}
}
