// Package diag contains the checks that wx can perform without an active
// daemon connection.  The daemon and the CLI use the same implementation so
// a failed connection does not replace useful local facts with a second,
// subtly different diagnostic path.
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

// SharedChecks runs all checks whose answer does not require the daemon's
// SQLite store.  configError is supplied by the caller because the daemon can
// retain its last valid configuration after a failed reload, while the CLI
// obtains its status from config.Load directly.
func SharedChecks(ctx context.Context, cfg config.Config, configError string, runners ...*gitx.Runner) map[string]any {
	return sharedChecks(ctx, cfg, configError, false, runners...)
}

// SharedChecksWithoutLaunchAgent is used during the daemon's pending restart
// window.  The old process cannot render the incoming binary's plist, so even
// reading the comparison result in that window would create a false stale
// warning.
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

// LocalChecks returns the shared checks plus stable placeholders for the
// daemon-only checks.  reason is recorded under checks.daemon so callers can
// see why the fallback was selected while retaining the daemon command's
// non-zero exit status.
func LocalChecks(ctx context.Context, reason error) map[string]any {
	cfg, err := config.Load()
	configError := ""
	if err != nil {
		configError = err.Error()
		// Keep path diagnostics useful even when the configured file is invalid.
		// Defaults are the same effective values used before a first config load.
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

// DiagnosticPath reports whether path is the expected non-symlink kind and
// permission.  Exact permissions are intentional because every path checked
// here is private per-user state or a private worktree root.
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
