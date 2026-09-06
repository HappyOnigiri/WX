package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/pool"
	"github.com/HappyOnigiri/WX/internal/state"
	buildversion "github.com/HappyOnigiri/WX/internal/version"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

func (m *Manager) Status(ctx context.Context) (map[string]any, error) {
	s, err := m.store.Status(ctx)
	if err != nil {
		return nil, err
	}
	details, err := m.store.StatusDiagnostics(ctx)
	if err != nil {
		return nil, err
	}
	standby, err := m.standbyReplenishmentReport(ctx)
	if err != nil {
		return nil, err
	}
	m.mu.RLock()
	reloadAt, reloadError, backupAt, backupError := m.lastReload, m.reloadError, m.lastBackup, m.backupError
	rootError := m.rootError
	restartPending, stopPending := m.restartPending, m.stopPending
	cfg := m.cfg
	usage := make(map[string]rootUsageSample, len(m.rootUsage))
	for root, sample := range m.rootUsage {
		usage[root] = sample
	}
	m.mu.RUnlock()
	roots := m.knownRoots(ctx)
	for index := range details.Repositories {
		details.Repositories[index].Hot = false
		if leasedAt, parseErr := time.Parse(time.RFC3339Nano, details.Repositories[index].LastUsedAt); parseErr == nil {
			expiresAt := leasedAt.Add(cfg.Retention.HotStandby.Duration)
			details.Repositories[index].StandbyExpiresAt = state.FormatTime(expiresAt)
			details.Repositories[index].Hot = time.Now().Before(expiresAt)
		}
	}
	for index := range details.Sessions {
		if createdAt, parseErr := time.Parse(time.RFC3339Nano, details.Sessions[index].CreatedAt); parseErr == nil {
			details.Sessions[index].AgeSeconds = int64(time.Since(createdAt).Seconds())
		}
	}
	type rootStatus struct {
		Path           string `json:"path"`
		Active         bool   `json:"active"`
		Bytes          int64  `json:"bytes"`
		AllocatedBytes int64  `json:"allocated_bytes"`
		UnmanagedBytes int64  `json:"unmanaged_allocated_bytes"`
		Measurement    string `json:"measurement"`
		MeasuredAt     string `json:"measured_at,omitempty"`
		Error          string `json:"error,omitempty"`
	}
	// 使用量は lifecycle が測った値を返すだけにする。要求経路で walk すると root 配下の総ファイル数に比例して Status が遅くなる。
	rootStatuses := make([]rootStatus, 0, len(roots))
	for root, active := range roots {
		item := rootStatus{Path: root, Active: active, Measurement: rootUsagePendingMeasurement}
		if sample, measured := usage[root]; measured {
			item.Bytes, item.AllocatedBytes = sample.bytes, sample.allocated
			item.UnmanagedBytes = sample.unmanaged
			item.Measurement, item.MeasuredAt, item.Error = rootUsageMeasurement, state.FormatTime(sample.measuredAt), sample.err
		}
		rootStatuses = append(rootStatuses, item)
	}
	sort.Slice(rootStatuses, func(i, j int) bool { return rootStatuses[i].Path < rootStatuses[j].Path })
	return map[string]any{
		"schema_version": state.JSONSchemaVersion, "db_schema_version": state.SchemaVersion, "daemon_version": daemonVersion(), "protocol_version": 1, "uptime_seconds": int(time.Since(m.started).Seconds()),
		"pid":         os.Getpid(),
		"config_path": must(config.Path()), "config_last_reload": reloadAt.UTC().Format(time.RFC3339Nano), "config_reload_error": reloadError, "worktree_root_error": rootError,
		"sqlite_last_backup": formatOptionalTime(backupAt), "sqlite_backup_error": backupError, "restart_pending": restartPending, "stop_pending": stopPending,
		"workspaces": s.Workspaces, "repositories": s.Repositories,
		"slots":           map[string]int{"ready": s.Ready, "leased": s.Leased, "failed": s.Failed, "quarantined": s.Quarantined},
		"active_sessions": s.Active, "snapshots": s.Snapshots, "queued_jobs": s.Jobs, "worktree_roots": rootStatuses,
		"workspace_details": details.Workspaces, "session_details": details.Sessions, "repository_details": details.Repositories,
		"job_details": details.Jobs, "snapshot_details": details.Snapshots, "quarantine": details.Quarantine,
		"standby_replenishment": standby,
		"retention_seconds": map[string]int64{
			"hot_standby": cfg.Retention.HotStandby.Milliseconds() / 1000, "ended_worktree": cfg.Retention.EndedWorktree.Milliseconds() / 1000,
			"quarantined":       cfg.Retention.Quarantined.Milliseconds() / 1000,
			"recovery_snapshot": cfg.Retention.RecoverySnapshot.Milliseconds() / 1000, "expired_session_tombstone": cfg.Retention.ExpiredSessionTombstone.Milliseconds() / 1000,
			"failed_job": cfg.Retention.FailedJob.Milliseconds() / 1000, "event_log": cfg.Retention.EventLog.Milliseconds() / 1000,
		},
	}, nil
}

func daemonVersion() string {
	info, ok := debug.ReadBuildInfo()
	embedded, _ := buildversion.EmbeddedString()
	return daemonVersionForBuildInfo(info, ok, embedded)
}

func daemonVersionForBuildInfo(info *debug.BuildInfo, ok bool, embedded string) string {
	if ok {
		if info.Main.Version != "" && info.Main.Version != "(devel)" {
			return info.Main.Version
		}
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				return setting.Value
			}
		}
	}
	if embedded != "" {
		return embedded
	}
	if !ok {
		return "unknown"
	}
	return "devel"
}

func formatOptionalTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func (m *Manager) Doctor(ctx context.Context) map[string]any {
	m.mu.RLock()
	reloadError, restartPending, cfg := m.reloadError, m.restartPending, m.cfg
	rootError := m.rootError
	m.mu.RUnlock()
	var checks map[string]any
	if restartPending {
		checks = diag.SharedChecksWithoutLaunchAgent(ctx, cfg, reloadError, m.git)
	} else {
		checks = diag.SharedChecks(ctx, cfg, reloadError, m.git)
	}
	if restartPending {
		checks["launch_agent"] = "restart pending; LaunchAgent content check deferred"
	}

	if err := m.store.Ping(ctx); err != nil {
		checks["sqlite"] = err.Error()
	} else {
		checks["sqlite"] = "ok"
	}
	if rootError == "" {
		checks["worktree_root"] = "ok"
	} else {
		// 再起動しか手が無いと読ませないため、周期処理が再登録を試み続けることを添える。
		checks["worktree_root"] = rootError + "; wx retries the registration on each reconcile"
	}
	checks["worktree_registration"] = m.registrationDiagnostics(ctx)
	checks["artifact_ownership"] = m.artifactDiagnostics(ctx)
	standby, err := m.standbyReplenishmentReport(ctx)
	if err != nil {
		checks["standby_replenishment"] = err.Error()
	} else {
		checks["standby_replenishment"] = standby
	}
	return map[string]any{"schema_version": state.JSONSchemaVersion, "db_schema_version": state.SchemaVersion, "checks": checks}
}

func diagnosticPath(path string, requiredType os.FileMode, requiredPerm os.FileMode) string {
	return diag.DiagnosticPath(path, requiredType, requiredPerm)
}

func (m *Manager) registrationDiagnostics(ctx context.Context) map[string]any {
	result := map[string]any{"checked": 0, "invalid": []map[string]string{}}
	invalid := []map[string]string{}
	checked := 0
	roots, err := m.store.WorkspaceRoots(ctx)
	if err != nil {
		return map[string]any{"checked": 0, "error": err.Error()}
	}
	discoverer := discovery.Discoverer{Git: m.git, Config: m.Config()}
	for _, root := range roots {
		workspaceRecord, resolveErr := m.resolveRegisteredWorkspace(ctx, root, &discoverer)
		if resolveErr != nil {
			invalid = append(invalid, map[string]string{"workspace_root": root, "error": resolveErr.Error()})
			continue
		}
		resolved, resolveErr := pool.ResolveBranches(ctx, m.git, workspaceRecord, nil)
		if resolveErr != nil {
			invalid = append(invalid, map[string]string{"workspace_root": root, "error": resolveErr.Error()})
			continue
		}
		slots, slotsErr := m.store.ReadySlots(ctx, string(workspaceRecord.ID))
		if slotsErr != nil {
			invalid = append(invalid, map[string]string{"workspace_root": root, "error": slotsErr.Error()})
			continue
		}
		for _, slot := range slots {
			checked++
			valid, validationErr := m.readyMatches(ctx, slot, resolved)
			if validationErr != nil || !valid {
				detail := "READY invariants do not match current repository state"
				if validationErr != nil {
					detail = validationErr.Error()
				}
				invalid = append(invalid, map[string]string{"workspace_root": root, "slot_id": slot.ID, "path": slot.Path, "error": detail})
			}
		}
	}
	result["checked"] = checked
	result["invalid"] = invalid
	return result
}

// SlotView は slot 1 行に、lifecycle が測った使用量と、そこから決まるコピー方式を足したものである。
// 測定前や測定できない platform では Measurement がそれを示し、Bytes 系は実際の使用量ではない。
type SlotView struct {
	state.SlotSummary
	CopyMode       string `json:"copy_mode,omitempty"`
	Files          int    `json:"files,omitempty"`
	AllocatedBytes int64  `json:"allocated_bytes,omitempty"`
	SharedBytes    int64  `json:"shared_bytes,omitempty"`
	ExclusiveBytes int64  `json:"exclusive_bytes,omitempty"`
	Measurement    string `json:"measurement,omitempty"`
	MeasuredAt     string `json:"measured_at,omitempty"`
}

func (m *Manager) Slots(ctx context.Context, all bool) ([]SlotView, error) {
	summaries, err := m.store.ListSlots(ctx, all)
	if err != nil {
		return nil, err
	}
	m.mu.RLock()
	usage := make(map[string]slotUsageSample, len(m.slotUsage))
	for slotID, sample := range m.slotUsage {
		usage[slotID] = sample
	}
	m.mu.RUnlock()
	out := make([]SlotView, 0, len(summaries))
	for _, summary := range summaries {
		out = append(out, slotView(summary, usage))
	}
	return out, nil
}

// slotView は測定結果を 1 行へ畳み込む。共有と判定したファイルが 1 つでもあれば CoW が効いている slot とみなす。
func slotView(summary state.SlotSummary, usage map[string]slotUsageSample) SlotView {
	view := SlotView{SlotSummary: summary}
	if summary.SlotID == "" {
		return view
	}
	if !workspace.SharingSupported() {
		view.Measurement = slotSharingUnsupported
	}
	sample, measured := usage[summary.SlotID]
	if !measured {
		if view.Measurement == "" {
			view.Measurement = rootUsagePendingMeasurement
		}
		return view
	}
	view.Files = sample.usage.Files
	view.AllocatedBytes = sample.usage.AllocatedBytes
	view.SharedBytes = sample.usage.SharedBytes
	view.ExclusiveBytes = sample.usage.AllocatedBytes - sample.usage.SharedBytes
	view.MeasuredAt = state.FormatTime(sample.measuredAt)
	if view.Measurement == "" {
		view.Measurement = slotSharingMeasurement
		view.CopyMode = config.CopyModeCopy
		if sample.usage.SharedFiles > 0 {
			view.CopyMode = config.CopyModeCOW
		}
	}
	return view
}

func must(v string, e error) string {
	if e != nil {
		return ""
	}
	return v
}

func (m *Manager) artifactDiagnostics(ctx context.Context) map[string]any {
	unknownPaths, missingPaths := []string{}, []string{}
	unknownRefs, mismatchedRefs, missingRefs := []string{}, []string{}, []string{}
	diagnosticErrors := []string{}
	artifacts, err := m.store.SlotArtifacts(ctx)
	if err != nil {
		return map[string]any{"errors": []string{err.Error()}}
	}
	expectedPaths := map[string]state.SlotArtifact{}
	for _, artifact := range artifacts {
		clean := filepath.Clean(artifact.Path)
		expectedPaths[clean] = artifact
		if artifact.State == "ARCHIVED" || artifact.State == "REMOVING" {
			continue
		}
		exists, statErr := m.ownedPathExists(clean)
		if statErr != nil {
			diagnosticErrors = append(diagnosticErrors, fmt.Sprintf("inspect slot %s: %v", artifact.ID, statErr))
		} else if !exists {
			missingPaths = append(missingPaths, fmt.Sprintf("%s (%s, %s)", clean, artifact.ID, artifact.State))
		}
	}
	roots, rootsErr := m.rootPathsFromStore(ctx)
	if rootsErr != nil {
		diagnosticErrors = append(diagnosticErrors, fmt.Sprintf("list worktree root generations: %v", rootsErr))
	}
	for _, root := range roots {
		paths, pathsErr := m.ownedRootArtifactPaths(root)
		if pathsErr != nil {
			diagnosticErrors = append(diagnosticErrors, fmt.Sprintf("inspect root %s: %v", root, pathsErr))
			continue
		}
		for _, path := range paths {
			clean := filepath.Clean(path)
			if _, exists := expectedPaths[clean]; !exists {
				unknownPaths = append(unknownPaths, clean)
			}
		}
	}
	repositories, err := m.store.Repositories(ctx)
	if err != nil {
		diagnosticErrors = append(diagnosticErrors, err.Error())
	} else {
		for _, repository := range repositories {
			expectedList, refsErr := m.store.RecoveryRefExpectations(ctx, string(repository.ID))
			if refsErr != nil {
				diagnosticErrors = append(diagnosticErrors, fmt.Sprintf("read recovery refs for %s: %v", repository.ID, refsErr))
				continue
			}
			expected := map[string]state.RecoveryRefExpectation{}
			for _, ref := range expectedList {
				expected[ref.Ref] = ref
			}
			listed, listErr := m.git.Run(ctx, string(repository.MainPath), "for-each-ref", "--format=%(refname) %(objectname)", "refs/wx/recovery")
			if listErr != nil {
				diagnosticErrors = append(diagnosticErrors, fmt.Sprintf("list recovery refs for %s: %v", repository.ID, listErr))
				continue
			}
			actual := map[string]bool{}
			for _, line := range strings.Split(strings.TrimSpace(listed.Stdout), "\n") {
				fields := strings.Fields(line)
				if len(fields) == 0 {
					continue
				}
				if len(fields) != 2 {
					diagnosticErrors = append(diagnosticErrors, fmt.Sprintf("parse recovery ref listing for %s: %q", repository.ID, line))
					continue
				}
				ref, oid := fields[0], fields[1]
				actual[ref] = true
				want, known := expected[ref]
				switch {
				case !known:
					unknownRefs = append(unknownRefs, fmt.Sprintf("%s:%s", repository.ID, ref))
				case want.OID != oid:
					mismatchedRefs = append(mismatchedRefs, fmt.Sprintf("%s:%s", repository.ID, ref))
				}
			}
			for ref, expectation := range expected {
				if !actual[ref] && !expectation.InFlight {
					missingRefs = append(missingRefs, fmt.Sprintf("%s:%s", repository.ID, ref))
				}
			}
		}
	}
	for _, values := range [][]string{unknownPaths, missingPaths, unknownRefs, mismatchedRefs, missingRefs, diagnosticErrors} {
		sort.Strings(values)
	}
	return map[string]any{
		"unknown_paths": unknownPaths, "missing_paths": missingPaths,
		"unknown_refs": unknownRefs, "mismatched_refs": mismatchedRefs,
		"missing_refs": missingRefs, "errors": diagnosticErrors,
	}
}

// standbyReplenishmentReport は補充停止の診断へ復帰手順を付け、補充が有効な workspace だけに絞って返す。
// `wx clear` は補充の有無を問わず停止を記録するので、絞らないと `worktree: off` の workspace へ RetryStandby が拒否する案内を出してしまう。
// 停止行自体は残すため、後で `hot` へ戻した workspace には再び案内が出る。
func (m *Manager) standbyReplenishmentReport(ctx context.Context) ([]state.StandbyReplenishmentDiagnostic, error) {
	all, err := m.store.StandbyReplenishmentDiagnostics(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]state.StandbyReplenishmentDiagnostic, 0, len(all))
	for _, item := range all {
		if !m.standbyReplenishmentEnabledForRoot(item.Root) {
			continue
		}
		item.Action = "wx retry-standby " + strconv.Quote(item.Root)
		out = append(out, item)
	}
	return out, nil
}
