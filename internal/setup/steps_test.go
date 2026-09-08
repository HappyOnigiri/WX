package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/hookconfig"
	"github.com/HappyOnigiri/WX/internal/launchd"
)

func stepByID(t *testing.T, steps []Step, id string) Step {
	t.Helper()
	for _, step := range steps {
		if step.ID == id {
			return step
		}
	}
	t.Fatalf("step %s is missing", id)
	return Step{}
}

func TestWorktreeRootComparesRawValuesAndFixesPermissions(t *testing.T) {
	fixture := newSetupFixture(t)
	options := fixture.options()
	ctx := context.Background()

	step := collectWorktreeRoot()
	if step.State != StateAbsent || step.Desired != "$HOME/wx" {
		t.Fatalf("fresh worktree root=%+v", step)
	}
	if err := Apply(ctx, options, step, ActionInstall, ""); err != nil {
		t.Fatal(err)
	}
	// 実効値は展開・symlink 解決を経るため、raw の生文字列が保たれていることを確かめる。
	raw, err := config.LoadRaw()
	if err != nil {
		t.Fatal(err)
	}
	if raw.Storage.WorktreeRoot != "$HOME/wx" {
		t.Fatalf("config.yaml holds %q", raw.Storage.WorktreeRoot)
	}
	saved, err := os.ReadFile(filepath.Join(fixture.home, ".config", "wx", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "warm_per_workspace") {
		t.Fatalf("the effective configuration was baked into config.yaml:\n%s", saved)
	}
	if collectWorktreeRoot().State != StatePresent {
		t.Fatal("the worktree root is not present right after install")
	}

	// permission が緩い root は divergent として報告し、update で直す。
	if err := os.Chmod(filepath.Join(fixture.home, "wx"), 0o755); err != nil {
		t.Fatal(err)
	}
	loose := collectWorktreeRoot()
	if loose.State != StateDivergent || len(loose.Reasons) == 0 {
		t.Fatalf("loose permissions=%+v", loose)
	}
	if err := Apply(ctx, options, loose, ActionUpdate, ""); err != nil {
		t.Fatal(err)
	}
	if collectWorktreeRoot().State != StatePresent {
		t.Fatal("update did not fix the worktree root permissions")
	}
}

func TestWorktreeRootAcceptsAnEnteredPath(t *testing.T) {
	fixture := newSetupFixture(t)
	chosen := filepath.Join(fixture.home, "elsewhere")
	if err := Apply(context.Background(), fixture.options(), collectWorktreeRoot(), ActionInstall, chosen); err != nil {
		t.Fatal(err)
	}
	step := collectWorktreeRoot()
	if step.State != StatePresent || step.Current != chosen {
		t.Fatalf("entered path=%+v", step)
	}
}

func TestLaunchAgentReportsPermissionsAndStaleContent(t *testing.T) {
	fixture := newSetupFixture(t)
	options := fixture.options()
	ctx := context.Background()
	if collectLaunchAgent().State != StateAbsent {
		t.Fatal("a missing plist was not reported absent")
	}
	if err := Apply(ctx, options, collectLaunchAgent(), ActionInstall, ""); err != nil {
		t.Fatal(err)
	}
	if collectLaunchAgent().State != StatePresent {
		t.Fatal("the installed plist was not reported present")
	}
	path, err := launchd.PlistPath()
	if err != nil {
		t.Fatal(err)
	}
	// CurrentPlistStatus は byte 比較だけで permission を見ないため、両方を合成しないと doctor と矛盾する。
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	loose := collectLaunchAgent()
	if loose.State != StateDivergent || !strings.Contains(strings.Join(loose.Reasons, " "), "unsafe permissions") {
		t.Fatalf("loose plist permissions=%+v", loose)
	}
	if err := os.WriteFile(path, []byte("<plist/>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if stale := collectLaunchAgent(); stale.State != StateDivergent {
		t.Fatalf("stale plist=%+v", stale)
	}
	if err := Apply(ctx, options, collectLaunchAgent(), ActionRemove, ""); err != nil {
		t.Fatal(err)
	}
	if collectLaunchAgent().State != StateAbsent {
		t.Fatal("remove left the plist behind")
	}
	if err := Apply(ctx, Options{}, Step{ID: stepLaunchAgent}, ActionRemove, ""); err == nil {
		t.Fatal("removing the LaunchAgent without an adapter succeeded")
	}
	if err := Apply(ctx, Options{}, Step{ID: stepLaunchAgent}, ActionInstall, ""); err == nil {
		t.Fatal("installing the LaunchAgent without an adapter succeeded")
	}
}

func TestDaemonUsesAStatusRequestNotJustTheSocket(t *testing.T) {
	fixture := newSetupFixture(t)
	ctx := context.Background()
	if collectDaemon(ctx, fixture.options()).State != StateAbsent {
		t.Fatal("a stopped daemon was not reported absent")
	}
	if err := Apply(ctx, fixture.options(), Step{ID: stepDaemon}, ActionInstall, ""); err != nil {
		t.Fatal(err)
	}
	if collectDaemon(ctx, fixture.options()).State != StatePresent {
		t.Fatal("a running daemon was not reported present")
	}
	// 応答はあるが要求が通らない daemon は「いるが壊れている」ので divergent とする。
	broken := Options{DaemonStatus: func(context.Context) (bool, error) { return false, errors.New("damaged") }}
	if step := collectDaemon(ctx, broken); step.State != StateDivergent {
		t.Fatalf("a broken daemon=%+v", step)
	}
	if step := collectDaemon(ctx, Options{}); step.State != StateUnknown {
		t.Fatalf("a daemon without an adapter=%+v", step)
	}
	if err := Apply(ctx, Options{}, Step{ID: stepDaemon}, ActionInstall, ""); err == nil {
		t.Fatal("starting the daemon without an adapter succeeded")
	}
}

func TestHooksStepsFollowTheHookConfigStatus(t *testing.T) {
	fixture := newSetupFixture(t)
	ctx := context.Background()
	step := collectHooks("claude")
	if step.State != StateAbsent || step.Target == "" || step.Desired != fixture.binary {
		t.Fatalf("fresh hooks step=%+v", step)
	}
	if err := Apply(ctx, fixture.options(), step, ActionInstall, ""); err != nil {
		t.Fatal(err)
	}
	if collectHooks("claude").State != StatePresent || !hookconfig.Available("claude") {
		t.Fatal("installed hooks were not reported present")
	}
	if err := Apply(ctx, fixture.options(), collectHooks("claude"), ActionRemove, ""); err != nil {
		t.Fatal(err)
	}
	if collectHooks("claude").State != StateAbsent {
		t.Fatal("removed hooks were not reported absent")
	}
	// blocked（読めない・policy で無効）は質問せず理由だけを見せる。
	writeSetupFile(t, filepath.Join(fixture.home, ".codex", "config.toml"), "[features]\nhooks = false\n")
	blocked := collectHooks("codex")
	if blocked.State != StateUnknown || len(blocked.Options) != 0 || len(blocked.Reasons) == 0 {
		t.Fatalf("blocked hooks step=%+v", blocked)
	}
}

func TestHooksStepIsNotApplicableWithoutTheAgent(t *testing.T) {
	_ = newSetupFixture(t)
	t.Setenv("PATH", t.TempDir())
	step := collectHooks("claude")
	if step.State != StateNotApplicable || len(step.Options) != 0 {
		t.Fatalf("missing agent=%+v", step)
	}
}

func TestPrerequisitesWarnAboutDevelopmentBuilds(t *testing.T) {
	fixture := newSetupFixture(t)
	step := collectPrerequisites(context.Background())
	if step.State != StatePresent || step.Desired != fixture.binary {
		t.Fatalf("prerequisites=%+v", step)
	}
	if len(step.Options) != 0 {
		t.Fatal("the prerequisites step must not ask anything")
	}
	// 書き込む command と判定基準の wx が食い違う開発 build を警告する。
	other := filepath.Join(t.TempDir(), "wx")
	writeSetupFile(t, other, "#!/bin/sh\n")
	if err := os.Chmod(other, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(other)+":/usr/bin:/bin")
	warned := collectPrerequisites(context.Background())
	if !strings.Contains(strings.Join(warned.Reasons, " "), "install wx first") {
		t.Fatalf("a development build was not reported: %+v", warned)
	}
	t.Setenv("PATH", t.TempDir())
	missing := collectPrerequisites(context.Background())
	if missing.State != StateUnknown {
		t.Fatalf("a missing wx on PATH=%+v", missing)
	}
}

func writeSetupFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
