package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
)

func TestCommandDispatchRejectsMalformedAndUnavailableRequests(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// go test の stdin は端末ではないが、端末から実行すると /dev/tty は開けてしまう。判定だけを固定する。
	previousTerminal := setupIsTerminal
	setupIsTerminal = func(int) bool { return false }
	t.Cleanup(func() { setupIsTerminal = previousTerminal })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name string
		args []string
		want int
	}{
		{name: "empty", want: 2},
		{name: "unknown", args: []string{"not-a-command"}, want: 2},
		{name: "unknown agent", args: []string{"editor"}, want: 2},
		{name: "agent flag parse", args: []string{"--not-a-flag"}, want: 2},
		{name: "status extra argument", args: []string{"status", "extra"}, want: 2},
		{name: "status unavailable", args: []string{"status"}, want: 1},
		{name: "gc extra argument", args: []string{"gc", "extra"}, want: 2},
		{name: "gc unavailable", args: []string{"gc"}, want: 1},
		{name: "prune extra argument", args: []string{"prune", "extra"}, want: 2},
		{name: "prune unavailable", args: []string{"prune"}, want: 1},
		{name: "clear extra argument", args: []string{"clear", "extra"}, want: 2},
		{name: "clear unavailable", args: []string{"clear"}, want: 1},
		{name: "retry standby missing path", args: []string{"retry-standby"}, want: 2},
		{name: "retry standby unavailable", args: []string{"retry-standby", "/tmp/workspace"}, want: 1},
		{name: "retry standby all with path", args: []string{"retry-standby", "--all", "/tmp/workspace"}, want: 2},
		{name: "retry standby path with all", args: []string{"retry-standby", "/tmp/workspace", "--all"}, want: 2},
		{name: "retry standby all unavailable", args: []string{"retry-standby", "--all"}, want: 1},
		{name: "renamed clean command", args: []string{"clean"}, want: 2},
		{name: "config wrong arity", args: []string{"config", "a", "b", "c"}, want: 2},
		{name: "config invalid field", args: []string{"config", "unknown.field", "value"}, want: 1},
		{name: "resume missing id", args: []string{"resume"}, want: 2},
		{name: "resume invalid agent", args: []string{"resume", "session", "editor"}, want: 2},
		{name: "daemon wrong arity", args: []string{"daemon"}, want: 2},
		{name: "daemon unknown action", args: []string{"daemon", "unknown"}, want: 2},
		{name: "daemon restart extra argument", args: []string{"daemon", "restart", "extra"}, want: 2},
		{name: "daemon stop extra argument", args: []string{"daemon", "stop", "extra"}, want: 2},
		{name: "daemon start extra argument", args: []string{"daemon", "start", "extra"}, want: 2},
		// --foreground は start だけに意味がある。
		// 他の操作で受け入れると wx daemon stop --foreground を mode と誤認させる。
		{name: "daemon stop rejects foreground", args: []string{"daemon", "stop", "--foreground"}, want: 2},
		{name: "daemon restart rejects foreground", args: []string{"daemon", "restart", "--foreground"}, want: 2},
		{name: "daemon install rejects foreground", args: []string{"daemon", "install", "--foreground"}, want: 2},
		{name: "daemon restart unavailable", args: []string{"daemon", "restart"}, want: 1},
		// 中断済み context は missing socket を ConnectError と判定する前に RPC client へ届く。
		// そのため live context のように already stopped ではなく失敗を返す。
		{name: "daemon stop unavailable", args: []string{"daemon", "stop"}, want: 1},
		{name: "daemon start unavailable", args: []string{"daemon", "start"}, want: 1},
		{name: "hook missing event", args: []string{"hook"}, want: 2},
		{name: "setup extra argument", args: []string{"setup", "extra"}, want: 2},
		{name: "setup json without check", args: []string{"setup", "--json"}, want: 2},
		{name: "setup update with check", args: []string{"setup", "--update", "--check"}, want: 2},
		// 対話実行は端末が要る環境の問題なので 1 で終える。
		// --update は install.sh から自動起動されるため、同じ状況でも 0 で終えて案内だけを出す。
		{name: "setup without a terminal", args: []string{"setup"}, want: 1},
		{name: "setup update without a terminal", args: []string{"setup", "--update"}, want: 0},
		{name: "slots extra argument", args: []string{"slots", "extra"}, want: 2},
		{name: "slots unavailable", args: []string{"slots"}, want: 1},
		{name: "sessions unknown subcommand", args: []string{"sessions", "unknown"}, want: 2},
		{name: "discard recovery missing path", args: []string{"discard-recovery"}, want: 2},
		{name: "discard recovery extra argument", args: []string{"discard-recovery", "/tmp/workspace", "extra"}, want: 2},
		{name: "discard recovery unavailable", args: []string{"discard-recovery", "/tmp/workspace"}, want: 1},
		{name: "forget missing path", args: []string{"forget"}, want: 2},
		{name: "shell extra argument", args: []string{"shell", "extra"}, want: 2},
		{name: "shell unavailable", args: []string{"shell"}, want: 1},
		{name: "run without a command", args: []string{"run"}, want: 2},
		{name: "run unavailable", args: []string{"run", "--", "true"}, want: 1},
		{name: "new extra argument", args: []string{"new", "extra"}, want: 2},
		{name: "new unavailable", args: []string{"new"}, want: 1},
		{name: "release missing id", args: []string{"release"}, want: 2},
		{name: "release unavailable", args: []string{"release", "session"}, want: 1},
		{name: "forget unavailable", args: []string{"forget", "/tmp/workspace"}, want: 1},
		{name: "agent daemon unavailable", args: []string{"codex"}, want: 1},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := run(ctx, test.args); got != test.want {
				t.Fatalf("run(%v)=%d, want %d", test.args, got, test.want)
			}
		})
	}
}

func TestConfigCommandDisplaysDefaultsAndHelp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got := runConfig(context.Background(), []string{"--help"}); got != 0 {
		t.Fatalf("config help exit=%d", got)
	}
	if got := runConfig(context.Background(), nil); got != 0 {
		t.Fatalf("config display exit=%d", got)
	}
}

func TestConfigCommandListOperations(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	key := "sessions.paths.claude.sessions"

	if got := runConfig(context.Background(), []string{key, "--add", "~/custom-sessions"}); got != 0 {
		t.Fatalf("config --add exit=%d", got)
	}
	raw, err := config.LoadRaw()
	if err != nil {
		t.Fatal(err)
	}
	if got := raw.Sessions.Paths.Claude.Sessions; len(got) != 2 || got[1] != "~/custom-sessions" {
		t.Fatalf("added session paths=%v, want default and user notation", got)
	}

	if got := runConfig(context.Background(), []string{key, "--remove", filepath.Join(home, "custom-sessions")}); got != 0 {
		t.Fatalf("config --remove exit=%d", got)
	}
	raw, err = config.LoadRaw()
	if err != nil {
		t.Fatal(err)
	}
	if got := raw.Sessions.Paths.Claude.Sessions; len(got) != 1 || got[0] != "~/.claude/projects" {
		t.Fatalf("removed session paths=%v, want default path", got)
	}

	if got := runConfig(context.Background(), []string{key, "--reset"}); got != 0 {
		t.Fatalf("config --reset exit=%d", got)
	}
	raw, err = config.LoadRaw()
	if err != nil {
		t.Fatal(err)
	}
	if raw.Sessions.Paths.Claude.Sessions != nil {
		t.Fatalf("reset session paths=%v, want unset", raw.Sessions.Paths.Claude.Sessions)
	}
	effective := config.Merge(config.Defaults(), raw)
	if got := effective.Sessions.SessionPaths("claude"); len(got) != 1 || got[0] != "~/.claude/projects" {
		t.Fatalf("effective reset session paths=%v, want defaults", got)
	}

	if got := runConfig(context.Background(), []string{key, "--reset", "extra"}); got != 2 {
		t.Fatalf("config --reset with extra argument exit=%d, want 2", got)
	}

	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "custom-sessions") {
		t.Fatalf("reset config still contains custom path: %s", data)
	}
}

// scope 指定は show・set・reset・list を同じ引数の形で受け、未知キーは global 経路と同じ終了コード1にする。
func TestConfigCommandScopeOperations(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ctx := context.Background()
	target := filepath.Join(home, "multi")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := runConfig(ctx, []string{"--workspace", target}); got != 0 {
		t.Fatalf("workspace display exit=%d", got)
	}
	if got := runConfig(ctx, []string{"--workspace", target, "retention.hot_standby", "0s"}); got != 0 {
		t.Fatalf("workspace set exit=%d", got)
	}
	raw, err := config.LoadRaw()
	if err != nil {
		t.Fatal(err)
	}
	effective := config.Merge(config.Defaults(), raw)
	if err := config.NormalizePaths(&effective); err != nil {
		t.Fatal(err)
	}
	if hot, overridden := effective.HotStandbyForWorkspace(target); hot != 0 || !overridden {
		t.Fatalf("hot standby=%s overridden=%v, want the explicit zero", hot, overridden)
	}
	if got := runConfig(ctx, []string{"--workspace", target, "discovery.exclude", "--add", "build"}); got != 0 {
		t.Fatalf("workspace list add exit=%d", got)
	}
	if got := runConfig(ctx, []string{"--workspace", target, "retention.hot_standby", "--reset"}); got != 0 {
		t.Fatalf("workspace reset exit=%d", got)
	}
	raw, err = config.LoadRaw()
	if err != nil {
		t.Fatal(err)
	}
	// 兄弟の list 指定は道連れにならない。
	if got := raw.Workspaces[target]; got.Retention.HotStandby != nil || len(got.Discovery.Exclude) == 0 {
		t.Fatalf("workspace override=%+v, want only the reset key dropped", got)
	}
	if got := runConfig(ctx, []string{"--workspace", target, "bogus", "1"}); got != 1 {
		t.Fatalf("unknown workspace key exit=%d, want 1", got)
	}
	if got := runConfig(ctx, []string{"--workspace", target, "warm_count", "1", "extra"}); got != 2 {
		t.Fatalf("wrong arity exit=%d, want 2", got)
	}
	if got := runConfig(ctx, []string{"--workspace", target, "--repository", target}); got != 2 {
		t.Fatalf("combined scopes exit=%d, want 2", got)
	}
	// repository scope は repository の外を拒否する。
	if got := runConfig(ctx, []string{"--repository", target, "readiness.mode", "full"}); got != 1 {
		t.Fatalf("repository scope outside a repository exit=%d, want 1", got)
	}
}
