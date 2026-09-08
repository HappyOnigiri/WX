package setup

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
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
			data, err := launchd.Render(binary, f.home, logPath)
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
			return os.Remove(path)
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
		if err := Apply(ctx, options, step, step.Default, ""); err != nil {
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

func TestApplyRejectsUnknownStepsAndSkipsNoOps(t *testing.T) {
	ctx := context.Background()
	if err := Apply(ctx, Options{}, Step{ID: "nonsense"}, ActionKeep, ""); err != nil {
		t.Fatalf("keep is a no-op: %v", err)
	}
	if err := Apply(ctx, Options{}, Step{ID: "nonsense"}, ActionInstall, ""); err == nil {
		t.Fatal("an unknown step was applied")
	}
	if _, err := CollectStep(ctx, Options{}, "nonsense"); err == nil {
		t.Fatal("an unknown step was collected")
	}
}
