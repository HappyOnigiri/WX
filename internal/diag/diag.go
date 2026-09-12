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

// 検査名は `--json` の識別子であり、表示にもそのまま出す。
const (
	CheckConfig                   = "config"
	CheckGit                      = "git"
	CheckDaemon                   = "daemon"
	CheckSocket                   = "socket"
	CheckStateDatabase            = "state_database"
	CheckSQLite                   = "sqlite"
	CheckSQLiteBackup             = "sqlite_backup"
	CheckLaunchAgent              = "launch_agent"
	CheckWorktreeRoot             = "worktree_root"
	CheckWorktreeRootRegistration = "worktree_root_registration"
	CheckReadinessHooks           = "readiness_hooks"
	CheckWorktreeRegistration     = "worktree_registration"
	CheckStandbyReplenishment     = "standby_replenishment"
	CheckArtifactOwnership        = "artifact_ownership"
	CheckRecoveryJobs             = "recovery_jobs"
	CheckWorkspaceSnapshots       = "workspace_snapshots"
)

// daemonUnavailable は daemon の応答が無いために実施できない検査の原因である。
const daemonUnavailable = "the SQLite state of the wx daemon was unavailable, so this check could not run"

// SharedOptions は daemon の状態で変わる共通診断の振る舞いを指定する。
type SharedOptions struct {
	// RestartPending は daemon の再起動待ちを表す。旧 process は新 binary の plist を生成できず、比較すると誤った stale 警告になる。
	RestartPending bool
	// Git は daemon が使う Runner を共有する。nil なら設定の readiness timeout で作る。
	Git *gitx.Runner
}

// SharedFindings は daemon の SQLite store を必要としない診断を実行する。
// daemon は reload 失敗後も最後の有効設定を保持できるため configError は呼び出し側から渡す。
func SharedFindings(ctx context.Context, cfg config.Config, configError string, opts SharedOptions) []Finding {
	findings := []Finding{configFinding(configError), gitFinding(ctx, cfg, opts.Git)}
	findings = append(findings, socketFinding(), stateDatabaseFinding(), launchAgentFinding(opts.RestartPending))
	findings = append(findings, worktreeRootFinding(cfg))
	return append(findings, readinessHookFindings()...)
}

// LocalFindings は共通診断に、daemon が要る検査の未検査を添えて返す。
// reason は接続に失敗した理由で、nil なら応答が無かったことだけを報告する。
func LocalFindings(ctx context.Context, reason error) []Finding {
	cfg, err := config.Load()
	configError := ""
	if err != nil {
		configError = err.Error()
		// 設定ファイルが不正でも path 診断を有効にする。既定値は初回 load 前と同じ実効値を使う。
		cfg = config.Defaults()
	}
	cause := "the wx daemon is not responding on its socket"
	if reason != nil {
		cause = reason.Error()
	}
	findings := SharedFindings(ctx, cfg, configError, SharedOptions{})
	findings = append(findings, Finding{
		Check: CheckDaemon, Severity: SeverityProblem, Summary: "the wx daemon could not be reached",
		Target: pathOrEmpty(config.SocketPath), Cause: cause,
		Action: "run wx daemon start; if it is not installed as a LaunchAgent, run wx daemon install",
	})
	return append(findings, UncheckedFindings(CheckDaemon, append([]string{CheckSQLite}, StoreDependentChecks()...)...)...)
}

// StoreDependentChecks は daemon の SQLite state を読めないと実施できない検査の名前である。
// sqlite 自体は開けるかどうかを見る検査なので含めない。
func StoreDependentChecks() []string {
	return []string{
		CheckSQLiteBackup, CheckWorktreeRootRegistration, CheckWorktreeRegistration,
		CheckStandbyReplenishment, CheckArtifactOwnership, CheckRecoveryJobs, CheckWorkspaceSnapshots,
	}
}

// UncheckedFindings は checks を未検査として返す。
// dependsOn には原因として既に報告した検査名を渡し、同じ故障を独立した問題として重複表示させない。
func UncheckedFindings(dependsOn string, checks ...string) []Finding {
	findings := make([]Finding, 0, len(checks))
	for _, check := range checks {
		findings = append(findings, Finding{
			Check: check, Severity: SeverityUnchecked, Summary: "this check did not run",
			Cause: daemonUnavailable, Action: "fix the failure this check depends on, then run wx doctor again", DependsOn: dependsOn,
		})
	}
	return findings
}

// DegradedFindings は SQLite を開けずに読み取り専用で起動した daemon の診断を返す。
// 開けなかった理由と対処は sqlite の finding が持ち、他の store 依存の検査は未検査として並べる。
func DegradedFindings(ctx context.Context, databasePath string, openError error, previousLayout bool) []Finding {
	cfg, err := config.Load()
	configError := ""
	if err != nil {
		configError = err.Error()
		cfg = config.Defaults()
	}
	action := fmt.Sprintf("preserve %s for investigation and restore a verified backup from %s.backups, then restart the daemon", databasePath, databasePath)
	if previousLayout {
		action = fmt.Sprintf("stop the daemon and remove %s; wx creates the current layout on the next start", databasePath)
	}
	findings := SharedFindings(ctx, cfg, configError, SharedOptions{})
	findings = append(findings, Finding{
		Check: CheckSQLite, Severity: SeverityProblem, Summary: "the daemon is running read-only because its SQLite state is unavailable",
		Target: databasePath, Cause: openError.Error(), Action: action,
	})
	return append(findings, UncheckedFindings(CheckSQLite, StoreDependentChecks()...)...)
}

func configFinding(configError string) Finding {
	target := pathOrEmpty(config.Path)
	if configError == "" {
		return Finding{Check: CheckConfig, Severity: SeverityOK, Summary: "the configuration is loaded", Target: target}
	}
	return Finding{
		Check: CheckConfig, Severity: SeverityProblem, Summary: "the configuration could not be loaded",
		Target: target, Cause: configError,
		Action: "fix the reported entry in the configuration file, then run wx config reload",
	}
}

func gitFinding(ctx context.Context, cfg config.Config, shared *gitx.Runner) Finding {
	runner := shared
	if runner == nil {
		runner = &gitx.Runner{Timeout: cfg.MaxReadinessTimeout()}
	}
	result, err := runner.Run(ctx, "", "--version")
	if err != nil {
		return Finding{
			Check: CheckGit, Severity: SeverityProblem, Summary: "git could not be executed", Target: "git",
			Cause:  err.Error(),
			Action: "install Git or fix PATH and the executable permission of git, then run wx doctor again",
		}
	}
	return Finding{
		Check: CheckGit, Severity: SeverityOK, Summary: "git is available", Target: "git",
		Details: []string{singleLine(result.Stdout)},
	}
}

// socketFinding は socket の種別と権限だけを問題として扱う。
// 欠損は daemon が起動時に作るものであり、接続できない事実は daemon の finding が報告する。
func socketFinding() Finding {
	path, err := config.SocketPath()
	if err != nil {
		return Finding{
			Check: CheckSocket, Severity: SeverityProblem, Summary: "the daemon socket path could not be resolved",
			Cause: err.Error(), Action: "check that HOME points to your home directory, then run wx doctor again",
		}
	}
	return pathFinding(pathSpec{
		check: CheckSocket, path: path, requiredType: os.ModeSocket, requiredPerm: 0o600,
		summary:       "the daemon socket is not usable",
		missing:       "the daemon socket does not exist; the daemon creates it when it starts",
		missingAction: "run wx daemon start if you expect the daemon to be running",
		repairAction:  "remove or fix the reported path so the daemon can bind its own socket, then run wx daemon start",
	})
}

// stateDatabaseFinding は state database の種別と権限だけを問題として扱う。
// 欠損は初回起動前の正常な状態であり、破損の疑いでも削除を案内しない。
func stateDatabaseFinding() Finding {
	path, err := config.StatePath()
	if err != nil {
		return Finding{
			Check: CheckStateDatabase, Severity: SeverityProblem, Summary: "the state database path could not be resolved",
			Cause: err.Error(), Action: "check that HOME points to your home directory, then run wx doctor again",
		}
	}
	return pathFinding(pathSpec{
		check: CheckStateDatabase, path: path, requiredType: 0, requiredPerm: 0o600,
		summary:       "the state database file is not usable",
		missing:       "the state database does not exist yet; the daemon creates it when it starts",
		missingAction: "run wx daemon start if you expect the daemon to be running",
		repairAction:  "restore the file type and its 0600 owner-only access; keep the current file for investigation instead of deleting it",
	})
}

func launchAgentFinding(restartPending bool) Finding {
	if restartPending {
		return Finding{
			Check: CheckLaunchAgent, Severity: SeverityInfo, Summary: "the LaunchAgent content check is deferred",
			Cause:  "a daemon restart is pending, and the running process cannot render the plist of the replacement binary",
			Action: "run wx doctor again once the replacement daemon is up",
		}
	}
	path, err := launchd.PlistPath()
	if err != nil {
		return Finding{
			Check: CheckLaunchAgent, Severity: SeverityProblem, Summary: "the LaunchAgent plist path could not be resolved",
			Cause: err.Error(), Action: "check that HOME points to your home directory, then run wx doctor again",
		}
	}
	if result, statErr := inspectPath(path, 0, 0o600); result != "ok" {
		if errors.Is(statErr, os.ErrNotExist) {
			return Finding{
				Check: CheckLaunchAgent, Severity: SeverityProblem, Summary: "the wx LaunchAgent is not installed",
				Target: path, Cause: "the LaunchAgent plist does not exist, so the daemon does not start on login",
				Action: "run wx daemon install",
			}
		}
		return Finding{
			Check: CheckLaunchAgent, Severity: SeverityProblem, Summary: "the LaunchAgent plist is not usable",
			Target: path, Cause: result, Action: "run wx daemon install to rewrite the plist with owner-only access",
		}
	}
	status, err := launchd.CurrentPlistStatus()
	switch status {
	case launchd.PlistCurrent:
		return Finding{Check: CheckLaunchAgent, Severity: SeverityOK, Summary: "the LaunchAgent plist matches this binary", Target: path}
	case launchd.PlistStale:
		return Finding{
			Check: CheckLaunchAgent, Severity: SeverityProblem, Summary: "the LaunchAgent plist does not match this binary",
			Target: path, Cause: "the installed plist differs from the one this wx binary renders, so login starts a different daemon",
			Action: "run wx daemon install",
		}
	case launchd.PlistUnknown:
		cause := "the installed plist could not be compared with the one this wx binary renders"
		if err != nil {
			cause = err.Error()
		}
		return Finding{
			Check: CheckLaunchAgent, Severity: SeverityUnchecked, Summary: "the LaunchAgent plist could not be compared",
			Target: path, Cause: cause, Action: "run wx daemon install to reinstall the plist from this binary",
		}
	}
	return Finding{
		Check: CheckLaunchAgent, Severity: SeverityUnchecked, Summary: "the LaunchAgent plist could not be compared",
		Target: path, Cause: "the plist comparison returned an unknown status", Action: "run wx daemon install to reinstall the plist from this binary",
	}
}

// worktreeRootFinding は設定された worktree root の path だけを検査する。
// 登録の成否は daemon が別の finding で報告し、どちらの結果も互いに上書きしない。
func worktreeRootFinding(cfg config.Config) Finding {
	root, err := config.ExpandHome(cfg.Storage.WorktreeRoot)
	if err != nil {
		return Finding{
			Check: CheckWorktreeRoot, Severity: SeverityProblem, Summary: "the configured worktree root could not be resolved",
			Target: cfg.Storage.WorktreeRoot, Cause: err.Error(),
			Action: "fix storage.worktree_root in the configuration file, then run wx config reload",
		}
	}
	return pathFinding(pathSpec{
		check: CheckWorktreeRoot, path: root, requiredType: os.ModeDir, requiredPerm: 0o700,
		summary:       "the worktree root is not usable",
		missing:       "the worktree root does not exist yet; wx creates it when it registers the root",
		missingAction: "run wx daemon start if you expect slots to be prepared",
		repairAction:  "fix the path, its 0700 owner-only access, or the mount it lives on; wx retries the registration on each reconcile",
	})
}

// readinessHookFindings は前面待機の要否だけを参考として返す。
// hook が無くても準備完了は同期的に判定できるため、必須の修復として扱わない。
func readinessHookFindings() []Finding {
	findings := make([]Finding, 0, 2)
	for _, agent := range []string{"claude", "codex"} {
		if hookconfig.Available(agent) {
			findings = append(findings, Finding{
				Check: CheckReadinessHooks, Severity: SeverityOK,
				Summary: "readiness hooks are configured", Target: agent,
			})
			continue
		}
		findings = append(findings, Finding{
			Check: CheckReadinessHooks, Severity: SeverityInfo,
			Summary: "readiness hooks are missing or invalid", Target: agent,
			Cause:  "the agent has no valid wx readiness hooks, so wx waits for readiness in the foreground",
			Action: "no action is required; configure the hooks only to skip the foreground wait",
		})
	}
	return findings
}

// pathSpec は path 検査 1 件の期待と、原因ごとの対処である。
type pathSpec struct {
	check, path, summary                 string
	missing, missingAction, repairAction string
	requiredType, requiredPerm           os.FileMode
}

// pathFinding は種別・権限の検査結果を finding へ写す。
// 欠損は起動前の正常な状態か、別の検査が報告する故障の結果なので、問題ではなく参考として返す。
func pathFinding(spec pathSpec) Finding {
	result, err := inspectPath(spec.path, spec.requiredType, spec.requiredPerm)
	switch {
	case result == "ok":
		return Finding{Check: spec.check, Severity: SeverityOK, Summary: "the path is usable", Target: spec.path}
	case errors.Is(err, os.ErrNotExist):
		return Finding{
			Check: spec.check, Severity: SeverityInfo, Summary: "the path does not exist", Target: spec.path,
			Cause: spec.missing, Action: spec.missingAction,
		}
	default:
		return Finding{
			Check: spec.check, Severity: SeverityProblem, Summary: spec.summary, Target: spec.path,
			Cause: result, Action: spec.repairAction,
		}
	}
}

// pathOrEmpty は表示用の path を返し、解決できない場合は対象を空にする。
func pathOrEmpty(resolve func() (string, error)) string {
	value, err := resolve()
	if err != nil {
		return ""
	}
	return value
}

// DiagnosticPath は path が想定した種別・権限の非 symlink かを返す。
// ここで調べるのは per-user の private state または private worktree root のため、権限は厳密に判定する。
func DiagnosticPath(path string, requiredType os.FileMode, requiredPerm os.FileMode) string {
	result, _ := inspectPath(path, requiredType, requiredPerm)
	return result
}

// inspectPath は判定結果に加えて Lstat の失敗を返す。
// 欠損と権限不足では対処が変わるため、呼び出し側は文面ではなく err で分岐する。
func inspectPath(path string, requiredType os.FileMode, requiredPerm os.FileMode) (string, error) {
	if path == "" {
		return "path unavailable", nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err.Error(), err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "unsafe symlink", nil
	}
	if requiredType == os.ModeDir && !info.IsDir() {
		return "not a directory", nil
	}
	if requiredType == os.ModeSocket && info.Mode()&os.ModeSocket == 0 {
		return "not a Unix socket", nil
	}
	if requiredType == 0 && !info.Mode().IsRegular() {
		return "not a regular file", nil
	}
	if info.Mode().Perm() != requiredPerm {
		return fmt.Sprintf("unsafe permissions %04o; expected %04o", info.Mode().Perm(), requiredPerm), nil
	}
	return "ok", nil
}
