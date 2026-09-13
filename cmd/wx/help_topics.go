package main

// help_topics.go は help の構成単位を持つ。usage 行と name 列は機械識別子なので訳さず、
// 説明だけを message ID で持ち、表示の直前に言語を解決する。
// 本文の生成と桁揃えは help.go の writeHelp が行う。

// helpRow はオプションやコマンドの 1 行である。name は原文のまま出す。
type helpRow struct {
	name string
	id   string
}

// helpBlock はヘルプ本文の 1 かたまりである。rows があれば定義の一覧、無ければ段落になる。
// blank は前に空行を置くかどうか、gap は最長の name と説明列の間隔である。
type helpBlock struct {
	id    string
	blank bool
	gap   int
	rows  []helpRow
}

// helpTopic は 1 コマンド分のヘルプである。
type helpTopic struct {
	usage  []string
	blocks []helpBlock
}

// helpTopics はコマンド名から本文を引く。"top" は wx 自身の help である。
var helpTopics = map[string]helpTopic{
	"top": {
		usage: []string{"wx [wx-options] <claude|codex> [agent-arguments...]", "wx <command> [options]"},
		blocks: []helpBlock{
			{blank: true, id: "help.top.p1"},
			{gap: 2, rows: []helpRow{
				{name: "--branch <branch|repo=branch>", id: "help.top.branch"},
				{name: "--fresh", id: "help.top.fresh"},
				{name: "-s, --select-worktree", id: "help.top.select_worktree"},
				{name: "-w, --worktree", id: "help.top.worktree"},
				{name: "-n, --no-worktree", id: "help.top.no_worktree"},
				{name: "-h, --help", id: "help.top.help"},
				{name: "-v, --version", id: "help.top.version"},
			}},
			{blank: true, id: "help.top.p2"},
			{gap: 1, rows: []helpRow{
				{name: "claude [arguments...]", id: "help.top.claude"},
				{name: "codex [arguments...]", id: "help.top.codex"},
				{name: "shell [--resume <id>]", id: "help.top.shell"},
				{name: "run [--resume <id>] -- <cmd>", id: "help.top.run"},
				{name: "new [--json]", id: "help.top.new"},
				{name: "release <id> [--discard]", id: "help.top.release"},
				{name: "status [--verbose] [--json]", id: "help.top.status"},
				{name: "doctor [--probe] [--json]", id: "help.top.doctor"},
				{name: "gc [--dry-run]", id: "help.top.gc"},
				{name: "prune [--all] [--dry-run]", id: "help.top.prune"},
				{name: "clear [--all] [--standby]", id: "help.top.clear"},
				{name: "retry-standby [--all] [<path>]", id: "help.top.retry_standby"},
				{name: "slots [--all] [--json]", id: "help.top.slots"},
				{name: "bench [--runs <n>] [--json]", id: "help.top.bench"},
				{name: "config [<key> ...]", id: "help.top.config"},
				{name: "setup [--check] [--remove]", id: "help.top.setup"},
				{name: "resume <id> [agent] [args...]", id: "help.top.resume"},
				{name: "discard-recovery <workspace>", id: "help.top.discard_recovery"},
				{name: "forget <workspace-path>", id: "help.top.forget"},
				{name: "daemon start|stop|restart", id: "help.top.daemon"},
				{name: "daemon install|uninstall", id: "help.top.daemon2"},
			}},
		},
	},
	"status": {
		usage: []string{"wx status [--verbose] [--json]"},
		blocks: []helpBlock{
			{blank: true, id: "help.status.p1"},
			{blank: true, id: "help.status.p2"},
			{blank: true, id: "help.status.p3"},
			{blank: true, id: "help.status.p4"},
			{gap: 2, rows: []helpRow{
				{name: "--verbose, -v", id: "help.status.verbose"},
				{name: "--json", id: "help.status.json"},
			}},
		},
	},
	"doctor": {
		usage: []string{"wx doctor [--probe] [--verbose] [--json]"},
		blocks: []helpBlock{
			{blank: true, id: "help.doctor.p1"},
			{blank: true, id: "help.doctor.p2"},
			{blank: true, id: "help.doctor.p3"},
			{blank: true, id: "help.doctor.p4"},
			{gap: 2, rows: []helpRow{
				{name: "--probe", id: "help.doctor.probe"},
				{name: "--verbose, -v", id: "help.doctor.verbose"},
				{name: "--json", id: "help.doctor.json"},
			}},
		},
	},
	"gc": {
		usage: []string{"wx gc [--dry-run]"},
		blocks: []helpBlock{
			{blank: true, id: "help.gc.p1"},
			{blank: true, id: "help.gc.p2"},
			{gap: 2, rows: []helpRow{
				{name: "--dry-run", id: "help.gc.dry_run"},
			}},
		},
	},
	"prune": {
		usage: []string{"wx prune [--all] [--dry-run]"},
		blocks: []helpBlock{
			{blank: true, id: "help.prune.p1"},
			{blank: true, id: "help.prune.p2"},
			{blank: true, id: "help.prune.p3"},
			{blank: true, id: "help.prune.p4"},
			{blank: true, id: "help.prune.p5"},
			{gap: 2, rows: []helpRow{
				{name: "--all", id: "help.prune.all"},
				{name: "--dry-run", id: "help.prune.dry_run"},
			}},
		},
	},
	"clear": {
		usage: []string{"wx clear [--all] [--standby] [--discard] [--dry-run]"},
		blocks: []helpBlock{
			{blank: true, id: "help.clear.p1"},
			{blank: true, id: "help.clear.p2"},
			{blank: true, id: "help.clear.p3"},
			{blank: true, id: "help.clear.p4"},
			{blank: true, id: "help.clear.p5"},
			{blank: true, id: "help.clear.p6"},
			{blank: true, id: "help.clear.p7"},
			{gap: 2, rows: []helpRow{
				{name: "--all", id: "help.clear.all"},
				{name: "--discard", id: "help.clear.discard"},
				{name: "--standby", id: "help.clear.standby"},
				{name: "--dry-run", id: "help.clear.dry_run"},
			}},
		},
	},
	"retry-standby": {
		usage: []string{"wx retry-standby <workspace-path>", "wx retry-standby --all"},
		blocks: []helpBlock{
			{blank: true, id: "help.retry_standby.p1"},
			{blank: true, id: "help.retry_standby.p2"},
			{blank: true, id: "help.retry_standby.p3"},
			{gap: 2, rows: []helpRow{
				{name: "--all", id: "help.retry_standby.all"},
			}},
		},
	},
	"config": {
		usage: []string{"wx config", "wx config --describe <key>", "wx config --system [<key> <value>|<key> --add <value>|<key> --remove <value>|<key> --reset]", "wx config --workspace-defaults [<key> <value>|<key> --add <value>|<key> --remove <value>|<key> --reset]", "wx config --repository-defaults [<key> <value>|<key> --add <value>|<key> --remove <value>|<key> --reset]", "wx config --workspace <root> [<key> <value>|<key> --add <value>|<key> --remove <value>|<key> --reset]", "wx config --workspace <root> --repository-defaults [<key> <value>|<key> --add <value>|<key> --remove <value>|<key> --reset]", "wx config --workspace <root> --repository <relative-path> [<key> <value>|<key> --add <value>|<key> --remove <value>|<key> --reset]"},
		blocks: []helpBlock{
			{blank: true, id: "help.config.p1"},
			{blank: true, id: "help.config.p2"},
			{blank: true, id: "help.config.p3"},
			{blank: true, id: "help.config.p4"},
			{blank: true, id: "help.config.p5"},
			{blank: true, id: "help.config.p6"},
			{blank: true, id: "help.config.p7"},
			{blank: true, id: "help.config.p8"},
			{blank: true, id: "help.config.p9"},
			{gap: 3, rows: []helpRow{
				{name: "always", id: "help.config.always"},
				{name: "worktree", id: "help.config.worktree"},
				{name: "off", id: "help.config.off"},
			}},
			{id: "help.config.p10"},
			{blank: true, id: "help.config.p11"},
			{gap: 2, rows: []helpRow{
				{name: "auto", id: "help.config.auto"},
				{name: "cow", id: "help.config.cow"},
				{name: "copy", id: "help.config.copy"},
			}},
			{id: "help.config.p12"},
			{blank: true, id: "help.config.p13"},
			{blank: true, id: "help.config.p14"},
			{gap: 2, rows: []helpRow{
				{name: "early", id: "help.config.early"},
				{name: "full", id: "help.config.full"},
			}},
			{id: "help.config.p15"},
		},
	},
	"resume": {
		usage: []string{"wx resume <wx-session-id> [claude|codex] [--fresh] [--branch <branch>] [agent-arguments...]"},
		blocks: []helpBlock{
			{blank: true, id: "help.resume.p1"},
		},
	},
	"shell": {
		usage: []string{"wx shell [--branch <branch|repo=branch>] [--resume <wx-session-id>]"},
		blocks: []helpBlock{
			{blank: true, id: "help.shell.p1"},
			{blank: true, id: "help.shell.p2"},
			{blank: true, id: "help.shell.p3"},
			{blank: true, id: "help.shell.p4"},
			{blank: true, id: "help.shell.p5"},
			{gap: 2, rows: []helpRow{
				{name: "--branch <branch|repo=branch>", id: "help.shell.branch"},
				{name: "--resume <wx-session-id>", id: "help.shell.resume"},
			}},
		},
	},
	"run": {
		usage: []string{"wx run [--branch <branch|repo=branch>] [--resume <wx-session-id>] -- <command> [arguments...]"},
		blocks: []helpBlock{
			{blank: true, id: "help.run.p1"},
			{blank: true, id: "help.run.p2"},
			{blank: true, id: "help.run.p3"},
			{blank: true, id: "help.run.p4"},
			{blank: true, id: "help.run.p5"},
			{gap: 2, rows: []helpRow{
				{name: "--branch <branch|repo=branch>", id: "help.run.branch"},
				{name: "--resume <wx-session-id>", id: "help.run.resume"},
			}},
		},
	},
	"new": {
		usage: []string{"wx new [--branch <branch|repo=branch>] [--json]"},
		blocks: []helpBlock{
			{blank: true, id: "help.new.p1"},
			{blank: true, id: "help.new.p2"},
			{blank: true, id: "help.new.p3"},
			{blank: true, id: "help.new.p4"},
			{blank: true, id: "help.new.p5"},
			{gap: 2, rows: []helpRow{
				{name: "--branch <branch|repo=branch>", id: "help.new.branch"},
				{name: "--json", id: "help.new.json"},
			}},
		},
	},
	"release": {
		usage: []string{"wx release <wx-session-id> [--discard]"},
		blocks: []helpBlock{
			{blank: true, id: "help.release.p1"},
			{blank: true, id: "help.release.p2"},
			{blank: true, id: "help.release.p3"},
			{blank: true, id: "help.release.p4"},
			{gap: 2, rows: []helpRow{
				{name: "--discard", id: "help.release.discard"},
			}},
		},
	},
	"slots": {
		usage: []string{"wx slots [--all] [--json]"},
		blocks: []helpBlock{
			{blank: true, id: "help.slots.p1"},
			{blank: true, id: "help.slots.p2"},
			{blank: true, id: "help.slots.p3"},
			{blank: true, id: "help.slots.p4"},
			{blank: true, id: "help.slots.p5"},
			{gap: 2, rows: []helpRow{
				{name: "--all", id: "help.slots.all"},
				{name: "--json", id: "help.slots.json"},
			}},
		},
	},
	"bench": {
		usage: []string{"wx bench [--runs <n>] [--branch <branch|repo=branch>] [--sweep|--config <key=value,...>] [--reuse] [--json]"},
		blocks: []helpBlock{
			{blank: true, id: "help.bench.p1"},
			{blank: true, id: "help.bench.p2"},
			{blank: true, id: "help.bench.p3"},
			{blank: true, id: "help.bench.p4"},
			{blank: true, id: "help.bench.p5"},
			{blank: true, id: "help.bench.p6"},
			{blank: true, id: "help.bench.p7"},
			{blank: true, id: "help.bench.p8"},
			{blank: true, id: "help.bench.p9"},
			{blank: true, id: "help.bench.p10"},
			{blank: true, id: "help.bench.p11"},
			{gap: 2, rows: []helpRow{
				{name: "--runs <n>", id: "help.bench.runs"},
				{name: "--branch <branch|repo=branch>", id: "help.bench.branch"},
				{name: "--sweep", id: "help.bench.sweep"},
				{name: "--config <key=value,...>", id: "help.bench.config"},
				{name: "--reuse", id: "help.bench.reuse"},
				{name: "--json", id: "help.bench.json"},
			}},
		},
	},
	"discard-recovery": {
		usage: []string{"wx discard-recovery <workspace-path> [--dry-run]"},
		blocks: []helpBlock{
			{blank: true, id: "help.discard_recovery.p1"},
			{blank: true, id: "help.discard_recovery.p2"},
			{blank: true, id: "help.discard_recovery.p3"},
			{gap: 2, rows: []helpRow{
				{name: "--dry-run", id: "help.discard_recovery.dry_run"},
			}},
		},
	},
	"forget": {
		usage: []string{"wx forget <workspace-path> [--discard-recovery]"},
		blocks: []helpBlock{
			{blank: true, id: "help.forget.p1"},
			{blank: true, id: "help.forget.p2"},
			{blank: true, id: "help.forget.p3"},
			{blank: true, id: "help.forget.p4"},
			{gap: 2, rows: []helpRow{
				{name: "--discard-recovery", id: "help.forget.discard_recovery"},
			}},
		},
	},
	"daemon": {
		usage: []string{"wx daemon <start|stop|restart|install|uninstall> [--foreground]"},
		blocks: []helpBlock{
			{blank: true, id: "help.daemon.p1"},
			{blank: true, gap: 2, rows: []helpRow{
				{name: "start", id: "help.daemon.start"},
				{name: "stop", id: "help.daemon.stop"},
				{name: "restart", id: "help.daemon.restart"},
				{name: "install", id: "help.daemon.install"},
				{name: "uninstall", id: "help.daemon.uninstall"},
			}},
			{blank: true, id: "help.daemon.p2"},
			{blank: true, id: "help.daemon.p3"},
			{gap: 2, rows: []helpRow{
				{name: "--foreground", id: "help.daemon.foreground"},
			}},
		},
	},
	"setup": {
		usage: []string{"wx setup [--check [--json]] [--update] [--remove]", "wx setup --item <id> --action <action> [--value <value>]"},
		blocks: []helpBlock{
			{blank: true, id: "help.setup.p1"},
			{blank: true, id: "help.setup.p2"},
			{blank: true, id: "help.setup.p3"},
			{blank: true, id: "help.setup.p4"},
			{blank: true, id: "help.setup.p5"},
			{blank: true, id: "help.setup.p6"},
			{gap: 2, rows: []helpRow{
				{name: "--check", id: "help.setup.check"},
				{name: "--json", id: "help.setup.json"},
				{name: "--update", id: "help.setup.update"},
				{name: "--remove", id: "help.setup.remove"},
				{name: "--item", id: "help.setup.item"},
				{name: "--action", id: "help.setup.action"},
				{name: "--value", id: "help.setup.value"},
			}},
		},
	},
	"hook": {
		usage: []string{"wx hook <event>"},
		blocks: []helpBlock{
			{blank: true, id: "help.hook.p1"},
		},
	},
}
