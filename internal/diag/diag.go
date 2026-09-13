// Package diag は daemon 接続なしで wx が実行できる診断を提供する。
// daemon と CLI が同じ実装を使い、接続失敗時もローカルの事実を保つ。
package diag

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/hookconfig"
	"github.com/HappyOnigiri/WX/internal/i18n"
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
	CheckUnsavedSubmodules        = "unsaved_submodules"
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
	findings := []Finding{configFinding(cfg, configError), gitFinding(ctx, cfg, opts.Git)}
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
		Messages: FindingMessages{
			Summary: i18n.Message{ID: "diag.daemon.unreachable"},
			Action:  i18n.Message{ID: "diag.action.daemon_start_install"},
		},
	})
	return append(findings, UncheckedFindings(CheckDaemon, append([]string{CheckSQLite}, StoreDependentChecks()...)...)...)
}

// StoreDependentChecks は daemon の SQLite state を読めないと実施できない検査の名前である。
// sqlite 自体は開けるかどうかを見る検査なので含めない。
func StoreDependentChecks() []string {
	return []string{
		CheckSQLiteBackup, CheckWorktreeRootRegistration, CheckWorktreeRegistration,
		CheckStandbyReplenishment, CheckArtifactOwnership, CheckRecoveryJobs, CheckWorkspaceSnapshots,
		CheckUnsavedSubmodules,
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
			Messages: FindingMessages{
				Summary: i18n.Message{ID: "diag.unchecked.summary"},
				Cause:   i18n.Message{ID: "diag.unchecked.cause"},
				Action:  i18n.Message{ID: "diag.action.fix_dependency"},
			},
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
	actionMessage := i18n.Message{ID: "diag.action.sqlite_restore", Data: map[string]any{"Path": databasePath}}
	if previousLayout {
		action = fmt.Sprintf("stop the daemon and remove %s; wx creates the current layout on the next start", databasePath)
		actionMessage = i18n.Message{ID: "diag.action.sqlite_remove", Data: map[string]any{"Path": databasePath}}
	}
	findings := SharedFindings(ctx, cfg, configError, SharedOptions{})
	findings = append(findings, Finding{
		Check: CheckSQLite, Severity: SeverityProblem, Summary: "the daemon is running read-only because its SQLite state is unavailable",
		Target: databasePath, Cause: openError.Error(), Action: action,
		Messages: FindingMessages{Summary: i18n.Message{ID: "diag.sqlite.degraded"}, Action: actionMessage},
	})
	return append(findings, UncheckedFindings(CheckSQLite, StoreDependentChecks()...)...)
}

// configFinding は設定の読み込み結果を報告する。読み込みに失敗した場合は未知キーを列挙できないため、
// その原因だけを返す。読めた場合でも wx が解釈しないキーが残っていれば、対象を示して問題として扱う。
func configFinding(cfg config.Config, configError string) Finding {
	target := pathOrEmpty(config.Path)
	if configError != "" {
		return Finding{
			Check: CheckConfig, Severity: SeverityProblem, Summary: "the configuration could not be loaded",
			Target: target, Cause: configError,
			Action: "correct the entry named in the cause in the configuration file, then run wx config reload",
			Messages: FindingMessages{
				Summary: i18n.Message{ID: "diag.config.load_failed"},
				Action:  i18n.Message{ID: "diag.action.fix_config_entry"},
			},
		}
	}
	if unknown := cfg.UnknownKeys(); len(unknown) > 0 {
		details := make([]string, 0, len(unknown))
		for _, key := range unknown {
			details = append(details, fmt.Sprintf("%s (line %d)", key.Key, key.Line))
		}
		return Finding{
			Check: CheckConfig, Severity: SeverityProblem,
			Summary: "the configuration was loaded, but it contains entries wx does not recognize",
			Target:  target, Cause: "wx ignored these entries: " + strings.Join(details, ", "),
			Action:  "correct the spelling of the reported entries or delete those lines, then run wx config reload",
			Details: details,
			Messages: FindingMessages{
				Summary: i18n.Message{ID: "diag.config.unknown_keys"},
				Cause:   i18n.Message{ID: "diag.config.ignored_entries", Data: map[string]any{"Entries": strings.Join(details, ", ")}},
				Action:  i18n.Message{ID: "diag.action.fix_unknown_keys"},
			},
		}
	}
	return Finding{
		Check: CheckConfig, Severity: SeverityOK, Summary: "the configuration is loaded", Target: target,
		Messages: FindingMessages{Summary: i18n.Message{ID: "diag.config.loaded"}},
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
			Messages: FindingMessages{
				Summary: i18n.Message{ID: "diag.git.failed"},
				Action:  i18n.Message{ID: "diag.action.install_git"},
			},
		}
	}
	return Finding{
		Check: CheckGit, Severity: SeverityOK, Summary: "git is available", Target: "git",
		Details:  []string{singleLine(result.Stdout)},
		Messages: FindingMessages{Summary: i18n.Message{ID: "diag.git.available"}},
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
			Messages: FindingMessages{
				Summary: i18n.Message{ID: "diag.socket.path_unresolved"},
				Action:  i18n.Message{ID: "diag.action.check_home"},
			},
		}
	}
	return pathFinding(pathSpec{
		check: CheckSocket, path: path, requiredType: os.ModeSocket, requiredPerm: 0o600,
		summary:       "the daemon socket is not usable",
		missing:       "the daemon socket does not exist; the daemon creates it when it starts",
		missingAction: "run wx daemon start if you expect the daemon to be running",
		repairAction:  "delete the file at the target path, or restore its Unix socket type and 0600 owner-only access, so the daemon can bind its own socket, then run wx daemon start",
		summaryID:     "diag.socket.unusable",
		missingID:     "diag.socket.missing",
		missingID2:    "diag.action.start_if_expected",
		repairID:      "diag.action.fix_socket_path",
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
			Messages: FindingMessages{
				Summary: i18n.Message{ID: "diag.state_db.path_unresolved"},
				Action:  i18n.Message{ID: "diag.action.check_home"},
			},
		}
	}
	return pathFinding(pathSpec{
		check: CheckStateDatabase, path: path, requiredType: 0, requiredPerm: 0o600,
		summary:       "the state database file is not usable",
		missing:       "the state database does not exist yet; the daemon creates it when it starts",
		missingAction: "run wx daemon start if you expect the daemon to be running",
		repairAction:  "restore the file type and its 0600 owner-only access; keep the current file for investigation instead of deleting it",
		summaryID:     "diag.state_db.unusable",
		missingID:     "diag.state_db.missing",
		missingID2:    "diag.action.start_if_expected",
		repairID:      "diag.action.restore_state_db",
	})
}

func launchAgentFinding(restartPending bool) Finding {
	if restartPending {
		return Finding{
			Check: CheckLaunchAgent, Severity: SeverityInfo, Summary: "the LaunchAgent content check is deferred",
			Cause:  "a daemon restart is pending, and the running process cannot render the plist of the replacement binary",
			Action: "run wx doctor again once the replacement daemon is up",
			Messages: FindingMessages{
				Summary: i18n.Message{ID: "diag.launch_agent.deferred"},
				Cause:   i18n.Message{ID: "diag.launch_agent.restart"},
				Action:  i18n.Message{ID: "diag.action.rerun_after_restart"},
			},
		}
	}
	path, err := launchd.PlistPath()
	if err != nil {
		return Finding{
			Check: CheckLaunchAgent, Severity: SeverityProblem, Summary: "the LaunchAgent plist path could not be resolved",
			Cause: err.Error(), Action: "check that HOME points to your home directory, then run wx doctor again",
			Messages: FindingMessages{
				Summary: i18n.Message{ID: "diag.launch_agent.unresolved"},
				Action:  i18n.Message{ID: "diag.action.check_home"},
			},
		}
	}
	if result, resultMessage, statErr := inspectPathDetail(path, 0, 0o600); result != "ok" {
		if errors.Is(statErr, os.ErrNotExist) {
			return Finding{
				Check: CheckLaunchAgent, Severity: SeverityProblem, Summary: "the wx LaunchAgent is not installed",
				Target: path, Cause: "the LaunchAgent plist does not exist, so the daemon does not start on login",
				Action: "run wx daemon install",
				Messages: FindingMessages{
					Summary: i18n.Message{ID: "diag.launch_agent.not_installed"},
					Cause:   i18n.Message{ID: "diag.launch_agent.plist_missing"},
					Action:  i18n.Message{ID: "diag.action.daemon_install"},
				},
			}
		}
		return Finding{
			Check: CheckLaunchAgent, Severity: SeverityProblem, Summary: "the LaunchAgent plist is not usable",
			Target: path, Cause: result, Action: "run wx daemon install to rewrite the plist with owner-only access",
			Messages: FindingMessages{
				Summary: i18n.Message{ID: "diag.launch_agent.plist_unusable"},
				Cause:   resultMessage,
				Action:  i18n.Message{ID: "diag.action.rewrite_plist"},
			},
		}
	}
	status, err := launchd.CurrentPlistStatus()
	switch status {
	case launchd.PlistCurrent:
		return Finding{
			Check: CheckLaunchAgent, Severity: SeverityOK, Summary: "the LaunchAgent plist matches this binary", Target: path,
			Messages: FindingMessages{Summary: i18n.Message{ID: "diag.launch_agent.plist_current"}},
		}
	case launchd.PlistStale:
		return Finding{
			Check: CheckLaunchAgent, Severity: SeverityProblem, Summary: "the LaunchAgent plist does not match this binary",
			Target: path, Cause: "the installed plist differs from the one this wx binary renders, so login starts a different daemon",
			Action: "run wx daemon install",
			Messages: FindingMessages{
				Summary: i18n.Message{ID: "diag.launch_agent.plist_stale"},
				Cause:   i18n.Message{ID: "diag.launch_agent.stale_cause"},
				Action:  i18n.Message{ID: "diag.action.daemon_install"},
			},
		}
	case launchd.PlistUnknown:
		cause := "the installed plist could not be compared with the one this wx binary renders"
		causeMessage := i18n.Message{ID: "diag.launch_agent.compare_cause"}
		if err != nil {
			cause, causeMessage = err.Error(), i18n.Message{}
		}
		return Finding{
			Check: CheckLaunchAgent, Severity: SeverityUnchecked, Summary: "the LaunchAgent plist could not be compared",
			Target: path, Cause: cause, Action: "run wx daemon install to reinstall the plist from this binary",
			Messages: FindingMessages{
				Summary: i18n.Message{ID: "diag.launch_agent.compare_failed"},
				Cause:   causeMessage,
				Action:  i18n.Message{ID: "diag.action.reinstall_plist"},
			},
		}
	}
	return Finding{
		Check: CheckLaunchAgent, Severity: SeverityUnchecked, Summary: "the LaunchAgent plist could not be compared",
		Target: path, Cause: "the plist comparison returned an unknown status", Action: "run wx daemon install to reinstall the plist from this binary",
		Messages: FindingMessages{
			Summary: i18n.Message{ID: "diag.launch_agent.compare_failed"},
			Cause:   i18n.Message{ID: "diag.launch_agent.unknown_status"},
			Action:  i18n.Message{ID: "diag.action.reinstall_plist"},
		},
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
			Messages: FindingMessages{
				Summary: i18n.Message{ID: "diag.worktree_root.unresolved"},
				Action:  i18n.Message{ID: "diag.action.fix_worktree_root"},
			},
		}
	}
	return pathFinding(pathSpec{
		check: CheckWorktreeRoot, path: root, requiredType: os.ModeDir, requiredPerm: 0o700,
		summary:       "the worktree root is not usable",
		missing:       "the worktree root does not exist yet; wx creates it when it registers the root",
		missingAction: "run wx daemon start if you expect slots to be prepared",
		repairAction:  "fix the path, its 0700 owner-only access, or the mount it lives on; wx retries the registration on each reconcile",
		summaryID:     "diag.worktree_root.unusable",
		missingID:     "diag.worktree_root.missing",
		missingID2:    "diag.action.start_for_slots",
		repairID:      "diag.action.fix_worktree_path",
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
				Messages: FindingMessages{Summary: i18n.Message{ID: "diag.hooks.configured"}},
			})
			continue
		}
		findings = append(findings, Finding{
			Check: CheckReadinessHooks, Severity: SeverityInfo,
			Summary: "readiness hooks are missing or invalid", Target: agent,
			Cause:  "the agent has no valid wx readiness hooks, so wx waits for readiness in the foreground",
			Action: "no action is required; configure the hooks only to skip the foreground wait",
			Messages: FindingMessages{
				Summary: i18n.Message{ID: "diag.hooks.missing"},
				Cause:   i18n.Message{ID: "diag.hooks.missing_cause"},
				Action:  i18n.Message{ID: "diag.action.hooks_optional"},
			},
		})
	}
	return findings
}

// pathSpec は path 検査 1 件の期待と、原因ごとの対処である。
type pathSpec struct {
	check, path, summary                 string
	missing, missingAction, repairAction string
	// summaryID・missingID・missingID2・repairID は同じ文の message ID である。
	// 表示は Resolve が解決し、JSON へ出る英語本文は上の文字列がそのまま担う。
	summaryID, missingID, missingID2, repairID string
	requiredType, requiredPerm                 os.FileMode
}

// pathFinding は種別・権限の検査結果を finding へ写す。
// 欠損は起動前の正常な状態か、別の検査が報告する故障の結果なので、問題ではなく参考として返す。
func pathFinding(spec pathSpec) Finding {
	result, resultMessage, err := inspectPathDetail(spec.path, spec.requiredType, spec.requiredPerm)
	switch {
	case result == "ok":
		return Finding{
			Check: spec.check, Severity: SeverityOK, Summary: "the path is usable", Target: spec.path,
			Messages: FindingMessages{Summary: i18n.Message{ID: "diag.path.ok"}},
		}
	case errors.Is(err, os.ErrNotExist):
		return Finding{
			Check: spec.check, Severity: SeverityInfo, Summary: "the path does not exist", Target: spec.path,
			Cause: spec.missing, Action: spec.missingAction,
			Messages: FindingMessages{
				Summary: i18n.Message{ID: "diag.path.missing"},
				Cause:   i18n.Message{ID: spec.missingID},
				Action:  i18n.Message{ID: spec.missingID2},
			},
		}
	default:
		return Finding{
			Check: spec.check, Severity: SeverityProblem, Summary: spec.summary, Target: spec.path,
			Cause: result, Action: spec.repairAction,
			Messages: FindingMessages{
				Summary: i18n.Message{ID: spec.summaryID},
				Cause:   resultMessage,
				Action:  i18n.Message{ID: spec.repairID},
			},
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

// DiagnosticPathMessage は DiagnosticPath の判定に、表示言語で解決する message を添えて返す。
// Lstat の失敗だけは外部由来の本文なので message を持たず、呼び出し側が原文を包む。
func DiagnosticPathMessage(path string, requiredType os.FileMode, requiredPerm os.FileMode) (string, i18n.Message) {
	result, message, _ := inspectPathDetail(path, requiredType, requiredPerm)
	return result, message
}

// inspectPath は判定結果に加えて Lstat の失敗を返す。
// 欠損と権限不足では対処が変わるため、呼び出し側は文面ではなく err で分岐する。
func inspectPath(path string, requiredType os.FileMode, requiredPerm os.FileMode) (string, error) {
	result, _, err := inspectPathDetail(path, requiredType, requiredPerm)
	return result, err
}

// inspectPathDetail は判定結果に、その表示文を解決する message も添えて返す。
// Lstat の失敗だけは外部由来の本文なので message を持たず、原文のまま表示する。
func inspectPathDetail(path string, requiredType os.FileMode, requiredPerm os.FileMode) (string, i18n.Message, error) {
	if path == "" {
		return "path unavailable", i18n.Message{ID: "diag.path.unavailable"}, nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err.Error(), i18n.Message{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "unsafe symlink", i18n.Message{ID: "diag.path.unsafe_symlink"}, nil
	}
	if requiredType == os.ModeDir && !info.IsDir() {
		return "not a directory", i18n.Message{ID: "diag.path.not_directory"}, nil
	}
	if requiredType == os.ModeSocket && info.Mode()&os.ModeSocket == 0 {
		return "not a Unix socket", i18n.Message{ID: "diag.path.not_socket"}, nil
	}
	if requiredType == 0 && !info.Mode().IsRegular() {
		return "not a regular file", i18n.Message{ID: "diag.path.not_regular"}, nil
	}
	if info.Mode().Perm() != requiredPerm {
		actual, expected := fmt.Sprintf("%04o", info.Mode().Perm()), fmt.Sprintf("%04o", requiredPerm)
		return fmt.Sprintf("unsafe permissions %s; expected %s", actual, expected),
			i18n.Message{ID: "diag.path.unsafe_permissions", Data: map[string]any{"Actual": actual, "Expected": expected}}, nil
	}
	return "ok", i18n.Message{}, nil
}
