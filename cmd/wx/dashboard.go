package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/HappyOnigiri/WX/internal/cli"
	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/dashboard"
	"github.com/HappyOnigiri/WX/internal/i18n"
	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/setup"
	"github.com/HappyOnigiri/WX/internal/update"
)

func runDashboard(ctx context.Context) int {
	ctx = commandContext(ctx)
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.T(ctx, "common.error", nil)+":", err)
		return 1
	}
	notice := ""
	for {
		// ダッシュボード内で language を変更した操作の後も、次の画面・RPC へ
		// 新しい設定を渡す。起動時の context は commandContext 済みでも更新する。
		ctx = i18n.WithLanguage(ctx, config.LoadLanguage())
		cfg, rawConfig, configErr := config.LoadWithRaw()
		if configErr != nil {
			// 不正設定でも診断や daemon 操作は使えるよう、設定タブだけを既定値で表示する。
			cfg, rawConfig = config.Defaults(), config.Config{}
			failure := i18n.T(ctx, "dashboard.config_load_failed", map[string]any{"Error": configErr.Error()})
			notice = i18n.T(ctx, "common.error", nil) + ": " + failure
		}
		addDashboardEnvironments(ctx, &cfg)
		steps, _ := setup.Collect(ctx, setupOptions())
		action, runErr := dashboard.Run(ctx, dashboard.Options{
			Status: dashboardStatus, CWD: cwd, Config: cfg, RawConfig: rawConfig, Setup: steps, Notice: notice,
			Execute: runDashboardInlineAction, Refresh: refreshDashboardState,
			Version: versionString(), Update: dashboardUpdate(ctx),
		})
		if errors.Is(runErr, dashboard.ErrCancelled) {
			return 0
		}
		if runErr != nil {
			fmt.Fprintln(os.Stderr, i18n.T(ctx, "common.error", nil)+": dashboard:", runErr)
			return 1
		}
		before, hadFingerprint := executableFingerprint()
		code := runDashboardAction(ctx, action)
		if action.Args[0] == "update" && updateReplacedBinary(before, hadFingerprint, code) {
			// この画面を動かしているのは置き換えられる前のバイナリなので、ループの先頭へは戻さない。
			fmt.Fprintln(os.Stderr, i18n.T(ctx, "wx.update.restart_dashboard", nil))
			return code
		}
		notice = i18n.T(ctx, "wx.dashboard.finished", map[string]any{"Command": action.Args[0], "Code": code})
	}
}

// executableFingerprint は実行中のバイナリの更新時刻と大きさを返す。
// install.sh は別の実体を書いて置き換えるため、実行の前後で比べると置き換えの有無が分かる。
// 実行ファイルを辿れない場合は false を返し、呼び出し側は終了コードだけで判断する。
func executableFingerprint() (string, bool) {
	path, err := os.Executable()
	if err != nil {
		return "", false
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	return strconv.FormatInt(info.ModTime().UnixNano(), 10) + "/" + strconv.FormatInt(info.Size(), 10), true
}

// updateReplacedBinary は update の実行がバイナリを置き換えたかを返す。
// 失敗して途中で終えた実行も、すでに最新で何もしなかった実行も置き換えていない。
// 置き換えていないのに再起動の案内を出すと、失敗の直後に成功の案内が続いて更新済みと誤解される。
func updateReplacedBinary(before string, hadFingerprint bool, code int) bool {
	if code != 0 {
		return false
	}
	after, ok := executableFingerprint()
	if !hadFingerprint || !ok {
		return true
	}
	return after != before
}

// updateStatusTimeout は確認結果の読み取りに与える上限である。
// 読むのは state の1行だけで、これは状態画面が開くより前に同期で呼ばれる。
// 使用量の集計まで含む statusDisplayTimeout を与えると、応答の遅い daemon で画面が長く出ない。
const updateStatusTimeout = 2 * time.Second

// dashboardUpdate は daemon が持つ確認結果を状態画面へ渡す。
// daemon が古くて method を知らない場合も、応答が得られない場合も、更新なしとして静かに扱う。
func dashboardUpdate(ctx context.Context) dashboard.UpdateInfo {
	c, err := rpcClient()
	if err != nil {
		return dashboard.UpdateInfo{}
	}
	callCtx, cancel := context.WithTimeout(ctx, updateStatusTimeout)
	defer cancel()
	var status daemon.UpdateStatus
	// 案内権は消費しない。状態画面の項目は常時表示で、対話起動の 1 回だけの案内とは役割が違う。
	if err := c.Call(callCtx, "UpdateStatus", rpc.UpdateStatusParams{ClaimAnnouncement: false}, &status); err != nil {
		return dashboard.UpdateInfo{}
	}
	if !status.Available || !update.Newer(versionString(), status.LatestVersion) {
		return dashboard.UpdateInfo{}
	}
	url := status.ReleaseURL
	if url == "" {
		url = update.ReleasesPage
	}
	return dashboard.UpdateInfo{Available: true, Version: status.LatestVersion, URL: url}
}

func refreshDashboardState(ctx context.Context) (config.Config, config.Config, []setup.Step, error) {
	cfg, rawConfig, err := config.LoadWithRaw()
	if err != nil {
		return config.Config{}, config.Config{}, nil, err
	}
	addDashboardEnvironments(ctx, &cfg)
	steps, err := setup.Collect(ctx, setupOptions())
	return cfg, rawConfig, steps, err
}

// addDashboardEnvironments は設定ファイルに書かれた workspace へ daemon 側の登録状況を重ねる。
// 環境一覧の対象は設定済みの workspace だけに保ち、貸出のたびに増える一時ディレクトリや
// 実体の消えた登録を設定対象として並べない。補う値は表示用で、設定ファイルへは保存しない。
func addDashboardEnvironments(ctx context.Context, cfg *config.Config) {
	if cfg == nil || len(cfg.Workspaces) == 0 {
		return
	}
	c, err := rpcClient()
	if err != nil {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, statusDisplayTimeout)
	defer cancel()
	var payload map[string]any
	if err := c.Call(callCtx, "Status", struct{}{}, &payload); err != nil {
		return
	}
	markDashboardRegistrations(cfg, payload)
}

// markDashboardRegistrations は Status 応答を設定済み workspace へ重ねる。
// daemon への接続を伴わない純粋な合成として分け、表示対象の決め方をテストで固定する。
func markDashboardRegistrations(cfg *config.Config, payload map[string]any) {
	workspaceDetails, _ := payload["workspace_details"].([]any)
	for _, raw := range workspaceDetails {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		root, _ := item["root"].(string)
		if root == "" {
			continue
		}
		workspace, configured := cfg.Workspaces[root]
		if !configured {
			continue
		}
		workspace.Discovered = true
		// membership は個別設定を書くまで設定ファイルに現れないため、設定済み workspace の配下に限って daemon の一覧から補う。
		members, _ := item["repositories"].([]any)
		if len(members) == 0 {
			members, _ = item["repository_memberships"].([]any)
		}
		for _, rawMembership := range members {
			membership, ok := rawMembership.(map[string]any)
			if !ok {
				continue
			}
			relative, _ := membership["relative_path"].(string)
			if relative == "" {
				continue
			}
			if workspace.Repositories == nil {
				workspace.Repositories = map[string]config.Repository{}
			}
			repository := workspace.Repositories[relative]
			repository.Discovered = true
			workspace.Repositories[relative] = repository
		}
		cfg.Workspaces[root] = workspace
	}
}

// runDashboardInlineAction は端末を引き渡さない CLI 操作を子 process で実行し、TUI の描画先と出力を分離する。
func runDashboardInlineAction(ctx context.Context, action dashboard.Action) (string, int) {
	ctx = commandContext(ctx)
	if len(action.Args) == 0 {
		return "", 0
	}
	binary, err := os.Executable()
	if err != nil {
		return i18n.T(ctx, "common.error", nil) + ": " + err.Error(), 1
	}
	command := exec.CommandContext(ctx, binary, action.Args...)
	if action.WorkDir != "" {
		command.Dir = action.WorkDir
	}
	output, err := command.CombinedOutput()
	text := strings.TrimSpace(string(output))
	if err == nil {
		return text, 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return text, exitErr.ExitCode()
	}
	if text != "" {
		text += "\n"
	}
	return text + i18n.T(ctx, "common.error", nil) + ": " + err.Error(), 1
}

func dashboardStatus(ctx context.Context) (string, error) {
	ctx = commandContext(ctx)
	c, err := rpcClient()
	if err != nil {
		return "", err
	}
	callCtx, cancel := context.WithTimeout(ctx, statusDisplayTimeout)
	defer cancel()
	var payload map[string]any
	if err := c.Call(callCtx, "Status", map[string]string{"language": string(i18n.LanguageFromContext(ctx))}, &payload); err != nil {
		return "", errors.New(rpcErrorMessageLanguage(err, i18n.LanguageFromContext(ctx)))
	}
	var out bytes.Buffer
	printStatusDisplay(&out, payload, false, i18n.LanguageFromContext(ctx))
	return out.String(), nil
}

func runDashboardAction(ctx context.Context, action dashboard.Action) int {
	if len(action.Args) == 0 {
		return 0
	}
	cwd := filepath.Clean(action.WorkDir)
	if action.WorkDir != "" {
		info, err := os.Stat(cwd)
		if err != nil || !info.IsDir() {
			fmt.Fprintln(os.Stderr, i18n.T(ctx, "common.error", nil)+":",
				i18n.T(ctx, "wx.dashboard.bad_target", map[string]any{"Target": strconv.Quote(action.WorkDir)}))
			return 1
		}
	}
	command, args := action.Args[0], action.Args[1:]
	switch command {
	case "claude", "codex":
		cfg, err := config.Load()
		if err != nil {
			fmt.Fprintln(os.Stderr, i18n.T(ctx, "common.error", nil)+":", err)
			return 1
		}
		client, err := cli.New(cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, i18n.T(ctx, "common.error", nil)+":", err)
			return 1
		}
		return client.RunAgentWithPolicyFrom(ctx, cwd, command, args, nil, false, cli.WorktreeOptions{SkipOnboarding: true})
	case "shell":
		return runShellFrom(ctx, args, cwd)
	case "run":
		return runRunFrom(ctx, args, cwd)
	case "new":
		return runNewFrom(ctx, args, cwd)
	case "bench":
		return runBenchFrom(ctx, args, cwd)
	default:
		return run(ctx, action.Args)
	}
}
