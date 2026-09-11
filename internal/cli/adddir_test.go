package cli

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
)

// leaseAddDirArgv は worktree 起動で組み立てる argv を、client.launch と同じ順序で返す。
func leaseAddDirArgv(mode string, lease daemon.Lease, args []string) []string {
	cfg := config.Defaults()
	cfg.Agent.AddDir = mode
	return addDirArgs(leaseAddDirs(cfg, lease), args)
}

func TestAddDirModesDecideWorktreeAndDirectLaunch(t *testing.T) {
	slot := filepath.Join(string(filepath.Separator)+"wx", "wsp001", "slt001")
	multi := daemon.Lease{Path: slot, RepositoryDirs: []string{"server", "web"}}
	withDirs := []string{"--add-dir", filepath.Join(slot, "server"), "--add-dir", filepath.Join(slot, "web"), "-p", "hello"}
	for _, test := range []struct {
		mode          string
		wantWorktree  []string
		wantDirectLen int
	}{
		{config.AgentAddDirAlways, withDirs, 2},
		{config.AgentAddDirWorktree, withDirs, 0},
		{config.AgentAddDirOff, []string{"-p", "hello"}, 0},
	} {
		if got := leaseAddDirArgv(test.mode, multi, []string{"-p", "hello"}); !reflect.DeepEqual(got, test.wantWorktree) {
			t.Errorf("%s worktree argv=%v", test.mode, got)
		}
		root := t.TempDir()
		for _, name := range []string{"server", "web"} {
			if err := os.MkdirAll(filepath.Join(root, name, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		t.Chdir(root)
		cfg := config.Defaults()
		cfg.Agent.AddDir = test.mode
		if got := directAddDirs(cfg, ""); len(got) != test.wantDirectLen {
			t.Errorf("%s direct dirs=%v", test.mode, got)
		}
	}
}

func TestAddDirSkipsSingleRepositoryWorkspaces(t *testing.T) {
	// 単一 repository の貸出は lease path が worktree そのもので、daemon は repository 名を返さない。
	single := daemon.Lease{Path: filepath.Join(string(filepath.Separator)+"wx", "wsp001", "slt001", "WX")}
	if got := leaseAddDirArgv(config.AgentAddDirAlways, single, []string{"-p"}); !reflect.DeepEqual(got, []string{"-p"}) {
		t.Errorf("single-repository worktree argv=%v", got)
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "internal", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := childRepositoryDirs(root); got != nil {
		t.Errorf("directory that is itself a repository=%v", got)
	}
}

func TestChildRepositoryDirsSkipsNonRepositoriesAndSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "server", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "server"), filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(root, "server")}
	if got := childRepositoryDirs(root); !reflect.DeepEqual(got, want) {
		t.Fatalf("dirs=%v", got)
	}
}

func TestAddDirArgsKeepsUserSuppliedDirectories(t *testing.T) {
	args := []string{"--add-dir", "/elsewhere", "-p"}
	want := []string{"--add-dir", "/slot/server", "--add-dir", "/elsewhere", "-p"}
	if got := addDirArgs([]string{"/slot/server"}, args); !reflect.DeepEqual(got, want) {
		t.Fatalf("argv=%v", got)
	}
}

// TestLaunchPassesRepositoryDirsToTheAgent は worktree 起動で実際に exec される argv を確かめる。
// leaseAddDirs 単体では、貸出コマンドを除く分岐と引数の順序が起動経路と一致していることを確かめられない。
func TestLaunchPassesRepositoryDirsToTheAgent(t *testing.T) {
	for _, test := range []struct {
		mode string
		want string
	}{
		{config.AgentAddDirAlways, "--add-dir {root}/server --add-dir {root}/web -p"},
		{config.AgentAddDirWorktree, "--add-dir {root}/server --add-dir {root}/web -p"},
		{config.AgentAddDirOff, "-p"},
	} {
		t.Run(test.mode, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			root := t.TempDir()
			record, eventLog := filepath.Join(root, "record"), filepath.Join(root, "events")
			t.Setenv("WX_TEST_LAUNCH_RECORD", record)
			t.Setenv("WX_TEST_EVENT_RECORD", eventLog)
			agent := writeLaunchRecorder(t, "claude")
			prependPath(t, filepath.Dir(agent))
			lease := daemon.Lease{SessionID: "test", Token: "token", Path: root, Ready: true, RepositoryDirs: []string{"server", "web"}}
			cfg := config.Defaults()
			cfg.Agent.AddDir = test.mode
			client, stop := serveResumeLaunchRPCWithConfig(t, &resumeLaunchHandler{lease: lease, eventLog: eventLog}, cfg)
			defer stop()
			if exit, retry := client.launch(context.Background(), launchPlan{agent: "claude", args: []string{"-p"}, cwd: root}); exit != 0 || retry {
				t.Fatalf("exit=%d retry=%t", exit, retry)
			}
			if got, want := readLaunchRecord(t, record)["args"], strings.ReplaceAll(test.want, "{root}", root); got != want {
				t.Fatalf("args=%q, want %q", got, want)
			}
		})
	}
}

// add_dir は workspace 個別指定を優先する。
// 貸出経路は daemon が返した canonical な source workspace を、直起動は解決した policy root をキーにする。
func TestAddDirFollowsWorkspaceOverride(t *testing.T) {
	slot := filepath.Join(string(filepath.Separator)+"wx", "wsp001", "slt001")
	lease := daemon.Lease{Path: slot, SourceWorkspace: "/src/multi", RepositoryDirs: []string{"server"}}
	cfg := config.Defaults()
	cfg.Agent.AddDir = config.AgentAddDirAlways
	cfg.Workspaces["/src/multi"] = config.Workspace{Agent: config.WorkspaceAgent{AddDir: config.AgentAddDirOff}}
	if got := leaseAddDirs(cfg, lease); got != nil {
		t.Fatalf("lease dirs=%v, want the workspace override to suppress them", got)
	}
	// 個別指定の無い workspace は global のまま。
	if got := leaseAddDirs(cfg, daemon.Lease{Path: slot, SourceWorkspace: "/src/other", RepositoryDirs: []string{"server"}}); len(got) != 1 {
		t.Fatalf("lease dirs=%v, want the global value", got)
	}

	root := t.TempDir()
	for _, name := range []string{"server", "web"} {
		if err := os.MkdirAll(filepath.Join(root, name, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(root)
	cfg.Workspaces["/src/direct"] = config.Workspace{Agent: config.WorkspaceAgent{AddDir: config.AgentAddDirWorktree}}
	if got := directAddDirs(cfg, "/src/direct"); got != nil {
		t.Fatalf("direct dirs=%v, want the workspace override to suppress them", got)
	}
	// policy root の解決に失敗した直起動は空の root で global へ落ち、起動そのものは続く。
	if got := directAddDirs(cfg, ""); len(got) != 2 {
		t.Fatalf("direct dirs=%v, want the global value when the root is unknown", got)
	}
}
