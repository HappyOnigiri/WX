package dashboard

import "github.com/HappyOnigiri/WorktreeX/internal/i18n"

// Action は TUI で確定した既存 CLI 操作である。
// WorkDir は起動・貸出・bench が対象にする明示的な作業元で、process の cwd は変更しない。
type Action struct {
	Args    []string
	WorkDir string
}

// menuItem の表示文は message ID で持つ。レイアウトの幅計算より前に言語を
// 解決しないと、padding を決めた後で表示幅が変わり列がずれる。
type menuItem struct {
	labelID         string
	descriptionID   string
	impactID        string
	command         string
	defaultArgs     []string
	inputLabelID    string
	inputNeeded     bool
	workDir         bool
	targetWorkspace bool
	targetAll       bool
	targetInput     bool
	argumentChoices func(*i18n.Localizer) []choice
	destructive     bool
	external        bool
}

var tabIDs = []string{
	"dashboard.tab.status",
	"dashboard.tab.launch",
	"dashboard.tab.settings",
	"dashboard.tab.doctor",
	"dashboard.tab.maintenance",
	"dashboard.tab.system",
}

// updateMenuItem は状態タブにだけ出る更新項目である。tabMenus[0] を作ると currentLabels と
// descriptionLines の既定分岐へ tab 0 が流れ、状態画面の代わりに operationView が出るため、ここへ単体で置く。
// 置き換えられる前のバイナリが TUI を動かしているので、実行は別 process へ渡す。
var updateMenuItem = menuItem{
	labelID: "menu.update.label", descriptionID: "menu.update.description", impactID: "menu.update.impact",
	command: "update", defaultArgs: []string{"--apply"}, external: true,
}

var tabMenus = map[int][]menuItem{
	1: {
		{labelID: "menu.claude.label", descriptionID: "menu.claude.description", impactID: "menu.claude.impact", command: "claude", inputLabelID: "menu.claude.input", workDir: true, argumentChoices: defaultOrCustomArguments("dashboard.default_launch"), external: true},
		{labelID: "menu.codex.label", descriptionID: "menu.codex.description", impactID: "menu.codex.impact", command: "codex", inputLabelID: "menu.codex.input", workDir: true, argumentChoices: defaultOrCustomArguments("dashboard.default_launch"), external: true},
		{labelID: "menu.resume.label", descriptionID: "menu.resume.description", impactID: "menu.resume.impact", command: "resume", inputLabelID: "menu.session_id.input", inputNeeded: true, external: true},
		{labelID: "menu.shell.label", descriptionID: "menu.shell.description", impactID: "menu.shell.impact", command: "shell", inputLabelID: "menu.shell.input", workDir: true, argumentChoices: defaultOrCustomArguments("dashboard.default_open"), external: true},
		{labelID: "menu.run.label", descriptionID: "menu.run.description", impactID: "menu.run.impact", command: "run", inputLabelID: "menu.run.input", inputNeeded: true, workDir: true, external: true},
		{labelID: "menu.new.label", descriptionID: "menu.new.description", impactID: "menu.new.impact", command: "new", inputLabelID: "menu.new.input", workDir: true, argumentChoices: defaultOrCustomArguments("dashboard.default_base")},
	},
	3: {
		{labelID: "menu.doctor.label", descriptionID: "menu.doctor.description", impactID: "menu.doctor.impact", command: "doctor"},
		{labelID: "menu.doctor_verbose.label", descriptionID: "menu.doctor_verbose.description", impactID: "menu.doctor_verbose.impact", command: "doctor", defaultArgs: []string{"--verbose"}},
		{labelID: "menu.doctor_probe.label", descriptionID: "menu.doctor_probe.description", impactID: "menu.doctor_probe.impact", command: "doctor", defaultArgs: []string{"--probe"}},
	},
	4: {
		{labelID: "menu.gc.label", descriptionID: "menu.gc.description", impactID: "menu.gc.impact", command: "gc", argumentChoices: dryRunChoices("dashboard.run_gc")},
		{labelID: "menu.clear.label", descriptionID: "menu.clear.description", impactID: "menu.clear.impact", command: "clear", inputLabelID: "menu.clear.input", argumentChoices: clearChoices, destructive: true},
		{labelID: "menu.prune.label", descriptionID: "menu.prune.description", impactID: "menu.prune.impact", command: "prune", inputLabelID: "menu.prune.input", argumentChoices: pruneChoices},
		{labelID: "menu.retry_standby.label", descriptionID: "menu.retry_standby.description", impactID: "menu.retry_standby.impact", command: "retry-standby", targetWorkspace: true, targetAll: true},
		{labelID: "menu.release.label", descriptionID: "menu.release.description", impactID: "menu.release.impact", command: "release", inputLabelID: "menu.session_id.input", inputNeeded: true, targetInput: true, argumentChoices: releaseChoices},
		{labelID: "menu.discard_recovery.label", descriptionID: "menu.discard_recovery.desc", impactID: "menu.discard_recovery.impact", command: "discard-recovery", targetWorkspace: true, argumentChoices: dryRunChoices("menu.discard_recovery.label"), destructive: true},
		{labelID: "menu.forget.label", descriptionID: "menu.forget.description", impactID: "menu.forget.impact", command: "forget", targetWorkspace: true, destructive: true},
		{labelID: "menu.bench.label", descriptionID: "menu.bench.description", impactID: "menu.bench.impact", command: "bench", inputLabelID: "menu.bench.input", workDir: true, argumentChoices: benchmarkChoices},
	},
	5: {
		{labelID: "menu.daemon_start.label", descriptionID: "menu.daemon_start.description", impactID: "menu.daemon_start.impact", command: "daemon", defaultArgs: []string{"start"}},
		{labelID: "menu.daemon_stop.label", descriptionID: "menu.daemon_stop.description", impactID: "menu.daemon_stop.impact", command: "daemon", defaultArgs: []string{"stop"}},
		{labelID: "menu.daemon_restart.label", descriptionID: "menu.daemon_restart.description", impactID: "menu.daemon_restart.impact", command: "daemon", defaultArgs: []string{"restart"}},
	},
}

func defaultOrCustomArguments(defaultID string) func(*i18n.Localizer) []choice {
	return func(l *i18n.Localizer) []choice {
		return []choice{
			{label: l.Localize(defaultID, nil)},
			{label: l.Localize("dashboard.custom_arguments", nil), input: true},
		}
	}
}

func dryRunChoices(runID string) func(*i18n.Localizer) []choice {
	return func(l *i18n.Localizer) []choice {
		return []choice{
			{label: l.Localize("dashboard.preview_changes", nil), value: "--dry-run"},
			{label: l.Localize(runID, nil)},
		}
	}
}

func clearChoices(l *i18n.Localizer) []choice {
	return []choice{
		{label: l.Localize("dashboard.clear_preview_default", nil), value: "--dry-run"},
		{label: l.Localize("dashboard.clear_default", nil)},
		{label: l.Localize("dashboard.clear_preview_standby", nil), value: "--standby --dry-run"},
		{label: l.Localize("dashboard.clear_standby", nil), value: "--standby"},
		{label: l.Localize("dashboard.clear_standby_refill", nil), value: "--standby --replenish"},
		{label: l.Localize("dashboard.custom_arguments", nil), input: true},
	}
}

func pruneChoices(l *i18n.Localizer) []choice {
	return []choice{
		{label: l.Localize("dashboard.prune_preview_safe", nil), value: "--dry-run"},
		{label: l.Localize("dashboard.prune_safe", nil)},
		{label: l.Localize("dashboard.prune_preview_all", nil), value: "--all --dry-run"},
		{label: l.Localize("dashboard.prune_all", nil), value: "--all"},
		{label: l.Localize("dashboard.custom_arguments", nil), input: true},
	}
}

func releaseChoices(l *i18n.Localizer) []choice {
	return []choice{
		{label: l.Localize("dashboard.release_save", nil)},
		{label: l.Localize("dashboard.release_discard", nil), value: "--discard"},
	}
}

func benchmarkChoices(l *i18n.Localizer) []choice {
	return []choice{
		{label: l.Localize("dashboard.bench_cold", nil)},
		{label: l.Localize("dashboard.bench_sweep", nil), value: "--sweep"},
		{label: l.Localize("dashboard.bench_reuse", nil), value: "--reuse"},
		{label: l.Localize("dashboard.custom_arguments", nil), input: true},
	}
}
