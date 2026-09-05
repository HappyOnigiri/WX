// Package diag は daemon 接続なしで wx が実行できる診断を提供する。
// daemon と CLI が同じ実装を使い、接続失敗時もローカルの事実を保つ。
package diag

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/hookconfig"
	"github.com/HappyOnigiri/WX/internal/launchd"
)

const daemonUnavailable = "unavailable without daemon"

// SharedChecks は daemon の SQLite store を必要としない診断を実行する。
// daemon は reload 失敗後も最後の有効設定を保持できるため configError は呼び出し側から渡す。
func SharedChecks(ctx context.Context, cfg config.Config, configError string, runners ...*gitx.Runner) map[string]any {
	return sharedChecks(ctx, cfg, configError, false, runners...)
}

// SharedChecksWithoutLaunchAgent は daemon の再起動待ちに使う。
// 旧 process は新 binary の plist を生成できず、比較結果を読むと誤った stale 警告になる。
func SharedChecksWithoutLaunchAgent(ctx context.Context, cfg config.Config, configError string, runners ...*gitx.Runner) map[string]any {
	return sharedChecks(ctx, cfg, configError, true, runners...)
}

func sharedChecks(ctx context.Context, cfg config.Config, configError string, skipLaunchAgent bool, runners ...*gitx.Runner) map[string]any {
	checks := map[string]any{}
	if configError == "" {
		checks["config"] = "ok"
	} else {
		checks["config"] = configError
	}

	runner := &gitx.Runner{Timeout: cfg.Readiness.Timeout.Duration}
	if len(runners) > 0 && runners[0] != nil {
		runner = runners[0]
	}
	if _, err := runner.Run(ctx, "", "--version"); err != nil {
		checks["git"] = err.Error()
	} else {
		checks["git"] = "ok"
	}

	checks["socket"] = pathCheck(config.SocketPath, os.ModeSocket, 0o600)
	checks["state_database"] = pathCheck(config.StatePath, 0, 0o600)
	if skipLaunchAgent {
		checks["launch_agent"] = "launch agent check deferred while daemon restart is pending"
	} else {
		checks["launch_agent"] = launchAgentCheck()
	}
	root, err := config.ExpandHome(cfg.Storage.WorktreeRoot)
	if err != nil {
		checks["worktree_root"] = err.Error()
	} else {
		checks["worktree_root"] = DiagnosticPath(root, os.ModeDir, 0o700)
	}
	checks["hooks"] = hookChecks()
	return checks
}

// LocalChecks は共通診断と daemon 専用診断の固定 placeholder を返す。
// fallback の理由を checks.daemon に記録し、daemon command の非ゼロ終了も保つ。
func LocalChecks(ctx context.Context, reason error) map[string]any {
	cfg, err := config.Load()
	configError := ""
	if err != nil {
		configError = err.Error()
		// 設定ファイルが不正でも path 診断を有効にする。既定値は初回 load 前と同じ実効値を使う。
		cfg = config.Defaults()
	}
	checks := SharedChecks(ctx, cfg, configError)
	checks["sqlite"] = daemonUnavailable
	checks["worktree_registration"] = map[string]any{"checked": 0, "error": daemonUnavailable}
	checks["artifact_ownership"] = map[string]any{"errors": []string{daemonUnavailable}}
	if reason == nil {
		checks["daemon"] = daemonUnavailable
	} else {
		checks["daemon"] = reason.Error()
	}
	return checks
}

func pathCheck(path func() (string, error), requiredType, requiredPerm os.FileMode) string {
	value, err := path()
	if err != nil {
		return err.Error()
	}
	return DiagnosticPath(value, requiredType, requiredPerm)
}

// DiagnosticPath は path が想定した種別・権限の非 symlink かを返す。
// ここで調べるのは per-user の private state または private worktree root のため、権限は厳密に判定する。
func DiagnosticPath(path string, requiredType os.FileMode, requiredPerm os.FileMode) string {
	if path == "" {
		return "path unavailable"
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err.Error()
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "unsafe symlink"
	}
	if requiredType == os.ModeDir && !info.IsDir() {
		return "not a directory"
	}
	if requiredType == os.ModeSocket && info.Mode()&os.ModeSocket == 0 {
		return "not a Unix socket"
	}
	if requiredType == 0 && !info.Mode().IsRegular() {
		return "not a regular file"
	}
	if info.Mode().Perm() != requiredPerm {
		return fmt.Sprintf("unsafe permissions %04o; expected %04o", info.Mode().Perm(), requiredPerm)
	}
	return "ok"
}

func launchAgentCheck() string {
	path, err := launchd.PlistPath()
	if err != nil {
		return err.Error()
	}
	if result := DiagnosticPath(path, 0, 0o600); result != "ok" {
		return result
	}
	status, err := launchd.CurrentPlistStatus()
	switch status {
	case launchd.PlistCurrent:
		return "ok"
	case launchd.PlistStale:
		return "stale LaunchAgent plist; run wx daemon install"
	case launchd.PlistUnknown:
		if err == nil {
			return "unable to compare LaunchAgent plist"
		}
		if errors.Is(err, os.ErrNotExist) {
			return "LaunchAgent plist is not installed"
		}
		return "unable to compare LaunchAgent plist: " + err.Error()
	}
	return "unable to compare LaunchAgent plist"
}

func hookChecks() map[string]string {
	hooks := map[string]string{}
	for _, agent := range []string{"claude", "codex"} {
		if hookconfig.Available(agent) {
			hooks[agent] = "ok"
		} else {
			hooks[agent] = "missing or invalid readiness hooks; foreground readiness remains required"
		}
	}
	return hooks
}
