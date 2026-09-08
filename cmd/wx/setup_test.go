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
	for _, want := range []string{`"pending": true`, `"id": "worktree_root"`, `"default": "default"`} {
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
	writeFakeLaunchAgent(t, home)
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
	writeFakeLaunchAgent(t, home)
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

// TestSetupStepValueReadsAPathOnlyForManual は manual を選んだときだけ入力を求めることを確認する。
// default では 1 行も読まない。読んでしまうと選択直後の Enter が path として解釈される。
func TestSetupStepValueReadsAPathOnlyForManual(t *testing.T) {
	var out, errOut bytes.Buffer
	step := setup.Step{ID: "worktree_root", Desired: "$HOME/wx", Options: []setup.Action{setup.ActionDefault, setup.ActionManual}, Default: setup.ActionDefault}
	entered := setupSession{
		out: &out, errOut: &errOut,
		readLine: func() (string, error) { return "  /tmp/wx-root  \n", nil },
	}
	value, err := setupStepValue(entered, step)
	if err != nil || value != "/tmp/wx-root" {
		t.Fatalf("value=%q err=%v", value, err)
	}
	if !strings.Contains(errOut.String(), "Enter the worktree root path [$HOME/wx]") {
		t.Fatalf("the prompt did not show the default: %q", errOut.String())
	}
	blank := setupSession{
		out: &out, errOut: &errOut,
		readLine: func() (string, error) { return "\n", nil },
	}
	value, err = setupStepValue(blank, step)
	if err != nil || value != "$HOME/wx" {
		t.Fatalf("blank input value=%q err=%v", value, err)
	}
	refusing := setupSession{
		out: &out, errOut: &errOut,
		readLine: func() (string, error) { t.Fatal("a step without a value read a line"); return "", nil },
	}
	if value, err := setupStepValue(refusing, setup.Step{ID: "launch_agent"}); err != nil || value != "" {
		t.Fatalf("a step without a value asked for one: %q %v", value, err)
	}
}

func readCommandFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestSetupRemoveReportsLeftoversInAParsableShape は uninstall.sh が読む leftover 行の形を守る。
// path には空白が入り得るため（`~/Library/Application Support/wx`）、行頭の目印と 2 列目以降が読み手の契約である。
func TestSetupRemoveReportsLeftoversInAParsableShape(t *testing.T) {
	home, options := setupCommandHome(t)
	removed := false
	options.UninstallLaunchAgent = func(context.Context) error { removed = true; return nil }
	writeFakeLaunchAgent(t, home)
	state := filepath.Join(home, "Library", "Application Support", "wx")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer

	if code := runSetupRemove(context.Background(), options, &out, &errOut); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if !removed {
		t.Fatal("--remove did not remove the LaunchAgent")
	}
	found := ""
	for _, line := range strings.Split(out.String(), "\n") {
		tag, rest, ok := strings.Cut(line, " ")
		if ok && tag == "leftover" {
			found = strings.TrimSpace(rest)
		}
	}
	if found != state {
		t.Fatalf("the state directory was not reported as a leftover: %q\n%s", found, out.String())
	}
}

// TestSetupRemoveReportsFailuresOnStderrAndExitsOne は失敗した項目だけを stderr へ出し、成功分の行を stdout に残すことを確認する。
func TestSetupRemoveReportsFailuresOnStderrAndExitsOne(t *testing.T) {
	home, options := setupCommandHome(t)
	options.UninstallLaunchAgent = func(context.Context) error { return errors.New("launchctl refused") }
	writeFakeLaunchAgent(t, home)
	var out, errOut bytes.Buffer

	if code := runSetupRemove(context.Background(), options, &out, &errOut); code != 1 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(errOut.String(), "error: launch_agent: launchctl refused") {
		t.Fatalf("the failure was not reported: %s", errOut.String())
	}
	if strings.Contains(out.String(), "launch_agent") {
		t.Fatalf("a failed item was printed as done:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "hooks.claude") {
		t.Fatalf("the items that succeeded are missing:\n%s", out.String())
	}
}

// TestSetupRejectsRemoveCombinedWithTheReadOnlyModes は --remove が --check・--update と混ざらないことを確認する。
// --check は何も変えない約束で、--remove は全部消す。取り違えは元に戻せない。
func TestSetupRejectsRemoveCombinedWithTheReadOnlyModes(t *testing.T) {
	setupCommandHome(t)
	for _, args := range [][]string{{"--remove", "--check"}, {"--remove", "--update"}, {"--remove", "extra"}} {
		if code := runSetup(context.Background(), args); code != 2 {
			t.Fatalf("wx setup %v exited %d", args, code)
		}
	}
}

// writeFakeLaunchAgent は plist を置く。--remove は plist が無い環境では launchctl を呼ばないため、
// 解除の経路を通すテストは実体を用意する必要がある。
func writeFakeLaunchAgent(t *testing.T, home string) {
	t.Helper()
	path := filepath.Join(home, "Library", "LaunchAgents", "com.user.wx.plist")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("<plist/>"), 0o600); err != nil {
		t.Fatal(err)
	}
}
