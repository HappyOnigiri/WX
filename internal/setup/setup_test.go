package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/i18n"
	"github.com/HappyOnigiri/WX/internal/launchd"
)

// setupFixture は制御した HOME と PATH を用意し、launchctl も daemon も動かさずに Apply を通す。
type setupFixture struct {
	home    string
	binary  string
	running bool
}

func newSetupFixture(t *testing.T) *setupFixture {
	t.Helper()
	fixture := &setupFixture{home: t.TempDir()}
	t.Setenv("HOME", fixture.home)
	t.Setenv("SHELL", "/bin/zsh")
	directory := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fixture.binary = filepath.Join(directory, "wx")
	if err := os.Symlink(executable, fixture.binary); err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{"claude", "codex"} {
		if err := os.WriteFile(filepath.Join(directory, agent), []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", directory+":/usr/bin:/bin")
	return fixture
}

// options は launchctl と socket を触らず、plist の内容と daemon の生死だけを再現する。
func (f *setupFixture) options() Options {
	return Options{
		InstallLaunchAgent: func(context.Context) error {
			binary, err := launchd.ResolveBinary()
			if err != nil {
				return err
			}
			logPath, err := config.LogPath()
			if err != nil {
				return err
			}
			data, err := launchd.Render(binary, f.home, logPath, launchd.LoginShellEnabled())
			if err != nil {
				return err
			}
			path, err := launchd.PlistPath()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			return os.WriteFile(path, data, 0o600)
		},
		UninstallLaunchAgent: func(context.Context) error {
			path, err := launchd.PlistPath()
			if err != nil {
				return err
			}
			// 未登録を成功として扱う。launchd.Uninstall も plist の ErrNotExist を呑むため、そこに揃える。
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return nil
		},
		StartDaemon:  func(context.Context) error { f.running = true; return nil },
		ReloadConfig: func(context.Context) error { return nil },
		DaemonStatus: func(context.Context) (bool, error) { return f.running, nil },
	}
}

// TestSetupIsIdempotent は要件の核である冪等性を確認する。
// 既定の操作を適用したあとは全項目が present か not_applicable になり、既定は keep へ落ち着く。
func TestSetupIsIdempotent(t *testing.T) {
	fixture := newSetupFixture(t)
	options := fixture.options()
	ctx := context.Background()
	steps, err := Collect(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	if !Pending(steps) {
		t.Fatal("a fresh environment reported nothing to do")
	}
	applyDefaults(t, ctx, options, steps)

	settled, err := Collect(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range settled {
		if step.State != StatePresent && step.State != StateNotApplicable {
			t.Fatalf("%s is %s after setup; reasons=%v", step.ID, step.State, step.Reasons)
		}
		if step.Default != ActionKeep {
			t.Fatalf("%s defaults to %s after setup", step.ID, step.Default)
		}
	}
	if Pending(settled) || len(Divergent(settled)) != 0 {
		t.Fatal("a completed setup still reports pending work")
	}

	before := snapshotSetupFiles(t, fixture.home)
	applyDefaults(t, ctx, options, settled)
	if after := snapshotSetupFiles(t, fixture.home); after != before {
		t.Fatalf("re-applying setup changed the files:\n%s\n%s", before, after)
	}
}

func applyDefaults(t *testing.T, ctx context.Context, options Options, steps []Step) {
	t.Helper()
	for _, step := range steps {
		if len(step.Options) == 0 {
			continue
		}
		if _, err := Apply(ctx, options, step, step.Default, ""); err != nil {
			t.Fatalf("apply %s %s: %v", step.ID, step.Default, err)
		}
	}
}

func snapshotSetupFiles(t *testing.T, home string) string {
	t.Helper()
	out := ""
	for _, relative := range []string{
		".config/wx/config.yaml", ".zshrc", "Library/LaunchAgents/com.user.wx.plist",
		".claude/settings.json", ".codex/hooks.json",
	} {
		data, err := os.ReadFile(filepath.Join(home, relative))
		if err != nil {
			out += relative + ": missing\n"
			continue
		}
		out += relative + ":\n" + string(data) + "\n"
	}
	return out
}

func TestOptionsForDerivesChoicesFromState(t *testing.T) {
	for _, test := range []struct {
		state    State
		options  []Action
		fallback Action
	}{
		{state: StateAbsent, options: []Action{ActionInstall, ActionSkip}, fallback: ActionInstall},
		{state: StatePresent, options: []Action{ActionKeep, ActionRemove}, fallback: ActionKeep},
		{state: StateDivergent, options: []Action{ActionUpdate, ActionKeep, ActionRemove}, fallback: ActionUpdate},
		{state: StateUnknown, fallback: ActionKeep},
		{state: StateNotApplicable, fallback: ActionKeep},
		{state: State("nonsense"), fallback: ActionKeep},
	} {
		t.Run(string(test.state), func(t *testing.T) {
			options, fallback := optionsFor(test.state)
			if fallback != test.fallback || len(options) != len(test.options) {
				t.Fatalf("optionsFor(%s)=%v,%s", test.state, options, fallback)
			}
			for index, option := range options {
				if option != test.options[index] {
					t.Fatalf("optionsFor(%s)=%v", test.state, options)
				}
			}
		})
	}
	// update を扱えない項目には update を出さない。keep だけになった項目は質問しない。
	if options, fallback := stepOptions(StateDivergent, worktreeRootActions); len(options) != 2 || fallback != ActionUpdate {
		t.Fatalf("worktree root divergent options=%v,%s", options, fallback)
	}
	if options, _ := stepOptions(StatePresent, []Action{ActionKeep}); options != nil {
		t.Fatalf("a keep-only step still asks: %v", options)
	}
	if options, fallback := stepOptions(StateAbsent, []Action{ActionSkip}); len(options) != 1 || fallback != ActionSkip {
		t.Fatalf("a skip-only step=%v,%s", options, fallback)
	}
}

// TestPendingRequiresAnAction は、未設定でも選択肢のない項目を pending と数えないことを確認する。
// unknown の項目などは状態だけが absent でも、利用者へ提示できる操作が無ければ進められない。
func TestPendingRequiresAnAction(t *testing.T) {
	if Pending([]Step{{State: StateAbsent}}) {
		t.Fatal("an absent step without options was reported pending")
	}
	if Pending([]Step{{State: StateDivergent}}) {
		t.Fatal("a divergent step without options was reported pending")
	}
	if !Pending([]Step{{State: StateAbsent, Options: []Action{ActionInstall}}}) {
		t.Fatal("an actionable absent step was not reported pending")
	}
}

func TestApplyRejectsUnknownStepsAndSkipsNoOps(t *testing.T) {
	ctx := context.Background()
	if _, err := Apply(ctx, Options{}, Step{ID: "nonsense"}, ActionKeep, ""); err != nil {
		t.Fatalf("keep is a no-op: %v", err)
	}
	if _, err := Apply(ctx, Options{}, Step{ID: "nonsense"}, ActionInstall, ""); err == nil {
		t.Fatal("an unknown step was applied")
	}
	if _, err := CollectStep(ctx, Options{}, "nonsense"); err == nil {
		t.Fatal("an unknown step was collected")
	}
}

// englishText は message を英語で解決する。表示文そのものを確かめるテストが使う。
func englishText(value i18n.Message) string {
	return i18n.New(string(i18n.English)).Message(value)
}

// englishJoin は理由の一覧を英語で 1 行にする。
func englishJoin(values []i18n.Message) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, englishText(value))
	}
	return strings.Join(parts, " ")
}

// hasMessageID は message ID の有無を返す。訳文の推敲でテストが壊れないよう、照合は ID で行う。
func hasMessageID(values []i18n.Message, id string) bool {
	for _, value := range values {
		if value.ID == id {
			return true
		}
	}
	return false
}

// TestStatesAndActionsHaveDisplayText は、state と action の機械値がそのまま画面へ出ないよう、
// 表示用の message が全ての値に揃っていることを守る。描画側は未知の値を機械値のまま出すため、
// この検査が無いと値を増やしたときだけ英語の識別子が画面に残る。
func TestStatesAndActionsHaveDisplayText(t *testing.T) {
	for _, state := range []State{StateAbsent, StatePresent, StateDivergent, StateUnknown, StateNotApplicable} {
		if id := "setup.state." + string(state); !i18n.HasMessage(id) {
			t.Errorf("message %q is missing; the state %q would be shown as a machine value", id, state)
		}
	}
	for _, action := range []Action{
		ActionInstall, ActionUpdate, ActionKeep, ActionRemove, ActionSkip,
		ActionDefault, ActionManual, ActionStart, ActionRestart,
	} {
		if id := "setup.action." + string(action); !i18n.HasMessage(id) {
			t.Errorf("message %q is missing; the action %q would be shown as a machine value", id, action)
		}
	}
}

// TestCollectReturnsUnresolvedMessages は、収集した項目が表示文ではなく message を返すことを守る。
// 見出しが欠けると画面のラベルが空になり、要約も理由も無い項目は表の DETAIL 列が空欄で並ぶ。
func TestCollectReturnsUnresolvedMessages(t *testing.T) {
	fixture := newSetupFixture(t)
	steps, err := Collect(context.Background(), fixture.options())
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) == 0 {
		t.Fatal("Collect returned no steps")
	}
	for _, step := range steps {
		if step.Title.ID == "" {
			t.Errorf("step %q has no title message", step.ID)
		}
		if step.Summary.ID == "" && step.Target == "" && len(step.Reasons) == 0 {
			t.Errorf("step %q has nothing to show in the detail column", step.ID)
		}
		if englishText(step.Title) == step.Title.ID {
			t.Errorf("step %q has a title id that is not in the catalog: %q", step.ID, step.Title.ID)
		}
	}
}
