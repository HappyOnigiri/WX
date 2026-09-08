package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/setup"
	"github.com/HappyOnigiri/WX/internal/tui"
)

// setupCommandHome は HOME と PATH を制御し、launchctl も daemon も動かさない Options を返す。
func setupCommandHome(t *testing.T) (string, setup.Options) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/zsh")
	directory := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(executable, filepath.Join(directory, "wx")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+":/usr/bin:/bin")
	return home, setup.Options{DaemonStatus: func(context.Context) (bool, error) { return true, nil }}
}

func TestSetupCheckReportsStateWithoutChangingAnything(t *testing.T) {
	home, options := setupCommandHome(t)
	var out, errOut bytes.Buffer
	if code := runSetupCheck(context.Background(), options, false, &out, &errOut); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	for _, want := range []string{"ITEM", "worktree_root", "launch_agent", "hooks.claude", "absent"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("check output is missing %q:\n%s", want, out.String())
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "wx", "config.yaml")); !os.IsNotExist(err) {
		t.Fatalf("--check wrote the configuration: %v", err)
	}
	var jsonOut bytes.Buffer
	if code := runSetupCheck(context.Background(), options, true, &jsonOut, &errOut); code != 0 {
		t.Fatalf("json exit=%d", code)
	}
	for _, want := range []string{`"pending": true`, `"id": "worktree_root"`, `"default": "install"`} {
		if !strings.Contains(jsonOut.String(), want) {
			t.Fatalf("json output is missing %q:\n%s", want, jsonOut.String())
		}
	}
	// state.JSONSchemaVersion は wx status / wx doctor の互換契約なので setup の payload には載せない。
	if strings.Contains(jsonOut.String(), "schema_version") {
		t.Fatalf("the setup payload carries a schema version:\n%s", jsonOut.String())
	}
}

// TestSetupUpdatePrintsNothingWhenNothingDiverged は install.sh から毎回走る経路を守る。
// 「変更なし」の 1 行が混ざるだけで通常の更新体験が壊れる。
func TestSetupUpdatePrintsNothingWhenNothingDiverged(t *testing.T) {
	_, options := setupCommandHome(t)
	var out, errOut bytes.Buffer
	calls := 0
	session := setupSession{out: &out, errOut: &errOut, selector: func(context.Context, setup.Step) (setup.Action, error) {
		calls++
		return setup.ActionKeep, nil
	}}
	if code := runSetupUpdate(context.Background(), options, session); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if out.Len() != 0 || errOut.Len() != 0 {
		t.Fatalf("--update wrote output: stdout=%q stderr=%q", out.String(), errOut.String())
	}
	if calls != 0 {
		t.Fatalf("--update asked %d question(s) with nothing divergent", calls)
	}
}

// TestSetupUpdateOffersOnlyTheDivergentStep は absent な項目を蒸し返さないことを確認する。
func TestSetupUpdateOffersOnlyTheDivergentStep(t *testing.T) {
	home, options := setupCommandHome(t)
	// LaunchAgent だけを divergent にする。plist の内容が今の wx と一致しない状態にあたる。
	plist := filepath.Join(home, "Library", "LaunchAgents", "com.user.wx.plist")
	if err := os.MkdirAll(filepath.Dir(plist), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plist, []byte("<plist/>"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	var asked []string
	installed := false
	options.InstallLaunchAgent = func(context.Context) error { installed = true; return nil }
	session := setupSession{out: &out, errOut: &errOut, selector: func(_ context.Context, step setup.Step) (setup.Action, error) {
		asked = append(asked, step.ID)
		return setup.ActionUpdate, nil
	}}
	if code := runSetupUpdate(context.Background(), options, session); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if len(asked) != 1 || asked[0] != "launch_agent" {
		t.Fatalf("asked=%v, want only launch_agent", asked)
	}
	if !installed {
		t.Fatal("the chosen update was not applied")
	}
	if !strings.Contains(errOut.String(), "no longer match") {
		t.Fatalf("the installer was not told what is being asked:\n%s", errOut.String())
	}
}

func TestSetupUpdateTreatsCancellationAsSuccessAndFailureAsOne(t *testing.T) {
	home, options := setupCommandHome(t)
	plist := filepath.Join(home, "Library", "LaunchAgents", "com.user.wx.plist")
	if err := os.MkdirAll(filepath.Dir(plist), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plist, []byte("<plist/>"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	cancelling := setupSession{out: &out, errOut: &errOut, selector: func(context.Context, setup.Step) (setup.Action, error) {
		return "", tui.ErrCancelled
	}}
	if code := runSetupUpdate(context.Background(), options, cancelling); code != 0 {
		t.Fatalf("cancelled --update exit=%d", code)
	}
	options.InstallLaunchAgent = func(context.Context) error { return errors.New("launchctl refused") }
	failing := setupSession{out: &out, errOut: &errOut, selector: func(context.Context, setup.Step) (setup.Action, error) {
		return setup.ActionUpdate, nil
	}}
	if code := runSetupUpdate(context.Background(), options, failing); code != 1 {
		t.Fatalf("failed --update exit=%d", code)
	}
	if !strings.Contains(errOut.String(), "launchctl refused") {
		t.Fatalf("the failure was not reported:\n%s", errOut.String())
	}
}

func TestSetupInteractiveAppliesDefaultsAndReportsCancellation(t *testing.T) {
	home, options := setupCommandHome(t)
	installed := 0
	options.InstallLaunchAgent = func(context.Context) error { installed++; return nil }
	var out, errOut bytes.Buffer
	session := setupSession{
		out: &out, errOut: &errOut,
		selector: func(_ context.Context, step setup.Step) (setup.Action, error) { return step.Default, nil },
		readLine: func() (string, error) { return "\n", nil },
	}
	if code := runSetupInteractive(context.Background(), options, session); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if installed != 1 {
		t.Fatalf("the LaunchAgent was installed %d time(s)", installed)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "wx", "config.yaml")); err != nil {
		t.Fatalf("the configuration was not written: %v", err)
	}
	if !strings.Contains(readCommandFile(t, filepath.Join(home, ".zshrc")), ".local/bin") {
		t.Fatal("the PATH block was not written")
	}
	if !strings.Contains(out.String(), "prerequisites") {
		t.Fatalf("the information-only step was not shown:\n%s", out.String())
	}

	cancelling := setupSession{out: &out, errOut: &errOut, selector: func(context.Context, setup.Step) (setup.Action, error) {
		return "", tui.ErrCancelled
	}}
	out.Reset()
	if code := runSetupInteractive(context.Background(), options, cancelling); code != 1 {
		t.Fatalf("cancelled setup exit=%d", code)
	}
	if !strings.Contains(out.String(), "items already applied are kept") {
		t.Fatalf("cancellation guidance is missing:\n%s", out.String())
	}
}

func TestSetupStepValueReadsAPathAfterTheSelector(t *testing.T) {
	_, options := setupCommandHome(t)
	var out, errOut bytes.Buffer
	step := setup.Step{ID: "worktree_root", Desired: "$HOME/wx", Options: []setup.Action{setup.ActionInstall, setup.ActionSkip}, Default: setup.ActionInstall}
	entered := setupSession{
		out: &out, errOut: &errOut,
		selector: func(_ context.Context, asked setup.Step) (setup.Action, error) {
			if asked.Title == "Worktree root path" {
				return setup.ActionUpdate, nil
			}
			return setup.ActionInstall, nil
		},
		readLine: func() (string, error) { return "  /tmp/wx-root  \n", nil },
	}
	value, err := setupStepValue(context.Background(), entered, step)
	if err != nil || value != "/tmp/wx-root" {
		t.Fatalf("value=%q err=%v", value, err)
	}
	kept := setupSession{
		out: &out, errOut: &errOut,
		selector: func(context.Context, setup.Step) (setup.Action, error) { return setup.ActionKeep, nil },
	}
	value, err = setupStepValue(context.Background(), kept, step)
	if err != nil || value != "$HOME/wx" {
		t.Fatalf("kept value=%q err=%v", value, err)
	}
	if value, err := setupStepValue(context.Background(), kept, setup.Step{ID: "launch_agent"}); err != nil || value != "" {
		t.Fatalf("a step without a value asked for one: %q %v", value, err)
	}
	_ = options
}

func readCommandFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
