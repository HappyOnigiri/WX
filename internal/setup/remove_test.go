package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
)

// TestRemoveUndoesSetup は setup が書いたものを Remove が消し、shell 起動ファイルだけを残すことを確認する。
func TestRemoveUndoesSetup(t *testing.T) {
	fixture := newSetupFixture(t)
	options := fixture.options()
	ctx := context.Background()
	steps, err := Collect(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	applyDefaults(t, ctx, options, steps)
	shell := filepath.Join(fixture.home, ".zshrc")
	before, err := os.ReadFile(shell)
	if err != nil {
		t.Fatal(err)
	}

	removal := Remove(ctx, options)
	for _, result := range removal.Results {
		if result.Err != nil {
			t.Fatalf("%s: %v", result.ID, result.Err)
		}
	}

	settled, err := Collect(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range settled {
		switch step.ID {
		// shell 起動ファイルは対象外なので present のままである。worktree_root は config.yaml を消した結果 absent に戻る。
		case stepShellPath, stepPrerequisites, stepDaemon:
			continue
		}
		if step.State != StateAbsent && step.State != StateNotApplicable {
			t.Fatalf("%s is %s after remove; reasons=%v", step.ID, step.State, step.Reasons)
		}
	}
	if after, err := os.ReadFile(shell); err != nil || string(after) != string(before) {
		t.Fatalf("remove touched the shell startup file: %v", err)
	}
	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s survived remove: %v", path, err)
	}
}

// TestRemoveIsIdempotent は 2 回目の Remove が失敗せず、hook 設定ファイルを作り直さないことを確認する。
// Remove は設定ファイルが無い agent で hookconfig.Remove を呼ばない。呼ぶと空の設定ファイルが新規作成される。
func TestRemoveIsIdempotent(t *testing.T) {
	fixture := newSetupFixture(t)
	options := fixture.options()
	ctx := context.Background()
	steps, err := Collect(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	applyDefaults(t, ctx, options, steps)
	if removal := Remove(ctx, options); removal.Failed() {
		t.Fatalf("the first remove failed: %+v", removal.Results)
	}
	for _, relative := range []string{".claude/settings.json", ".codex/hooks.json"} {
		if err := os.Remove(filepath.Join(fixture.home, relative)); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}

	removal := Remove(ctx, options)
	if removal.Failed() {
		t.Fatalf("the second remove failed: %+v", removal.Results)
	}
	for _, relative := range []string{".claude/settings.json", ".codex/hooks.json"} {
		if _, err := os.Stat(filepath.Join(fixture.home, relative)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("remove recreated %s: %v", relative, err)
		}
	}
}

// TestRemoveReportsLeftoversBeforeDeletingTheConfig は、消さない path を config.yaml の削除より前に集めることを確認する。
// worktree root は config.yaml が権威なので、後から集めると設定した path ではなく既定値を案内してしまう。
func TestRemoveReportsLeftoversBeforeDeletingTheConfig(t *testing.T) {
	fixture := newSetupFixture(t)
	options := fixture.options()
	ctx := context.Background()
	root := filepath.Join(fixture.home, "custom-root")
	step, err := CollectStep(ctx, options, stepWorktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyWorktreeRoot(ctx, options, step, ActionDefault, root); err != nil {
		t.Fatal(err)
	}
	state, err := config.StatePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(state), 0o700); err != nil {
		t.Fatal(err)
	}

	removal := Remove(ctx, options)
	if !slices.Contains(removal.Leftovers, root) {
		t.Fatalf("the configured worktree root is missing from %v", removal.Leftovers)
	}
	if !slices.Contains(removal.Leftovers, filepath.Dir(state)) {
		t.Fatalf("the state directory is missing from %v", removal.Leftovers)
	}
	// ログディレクトリは作っていないので案内しない。存在しない path を rm -rf の候補に出さない。
	logs, err := config.LogPath()
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(removal.Leftovers, filepath.Dir(logs)) {
		t.Fatalf("a missing log directory was reported: %v", removal.Leftovers)
	}
}

// TestRemoveKeepsGoingAfterAFailedItem は 1 項目の失敗で打ち切らないことを確認する。
// 途中で止めると、残った項目を消す手段が利用者に残らない。
func TestRemoveKeepsGoingAfterAFailedItem(t *testing.T) {
	fixture := newSetupFixture(t)
	options := fixture.options()
	ctx := context.Background()
	steps, err := Collect(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	applyDefaults(t, ctx, options, steps)
	options.UninstallLaunchAgent = func(context.Context) error { return errors.New("launchctl refused") }

	removal := Remove(ctx, options)
	if !removal.Failed() {
		t.Fatal("a failed LaunchAgent removal was reported as success")
	}
	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the configuration survived a failure in an earlier item: %v", err)
	}
}

// TestRemoveSkipsAnAbsentLaunchAgent は plist が無いときに launchctl を呼ばないことを確認する。
// 未登録の plist への bootout は `Boot-out failed: 5: Input/output error` になり、
// launchd 側が service の不在と判定できないため、呼べば --remove の 2 回目が失敗する。
func TestRemoveSkipsAnAbsentLaunchAgent(t *testing.T) {
	fixture := newSetupFixture(t)
	options := fixture.options()
	called := false
	options.UninstallLaunchAgent = func(context.Context) error { called = true; return nil }

	removal := Remove(context.Background(), options)
	if called {
		t.Fatal("remove asked launchctl to boot out a LaunchAgent that was never installed")
	}
	if removal.Failed() {
		t.Fatalf("removing a fresh environment failed: %+v", removal.Results)
	}
}
