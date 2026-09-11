package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/HappyOnigiri/WX/internal/state"
)

// missingArtifact は登録済み path の実体が無い slot である。
// 欠損の重大さは slot の状態で変わるため、表示側が判定できるよう state を残す。
type missingArtifact struct {
	SlotID, Path, State string
}

// recoveryRefIssue は recovery ref 1 件の不整合である。
// ExpiresAt は ref を支える snapshot の期限で、unknown ref（DB に記録が無い）では空になる。
type recoveryRefIssue struct {
	RepositoryID, Ref, ExpiresAt string
}

// key は reconcile・prune が使う `<repository_id>:<ref>` 形式を返す。
func (i recoveryRefIssue) key() string { return i.RepositoryID + ":" + i.Ref }

// unreadableRepository は refs を読めない repository 記録のうち、照合すべき snapshot を 1 件も持たないものである。
// GC の PruneRepositories が回収するまでの一時的な記録で、ownership error にすると回収までの間 doctor が失敗し続ける。
type unreadableRepository struct{ RepositoryID, Path, Cause string }

// artifactReport は worktree root と recovery ref の照合結果である。
// 表示側が「問題・参考・検査不能」を分けられる粒度で持ち、reconcile・prune が使う分類済み文字列は categories が作る。
type artifactReport struct {
	UnknownPaths   []string
	Missing        []missingArtifact
	UnknownRefs    []recoveryRefIssue
	MismatchedRefs []recoveryRefIssue
	MissingRefs    []recoveryRefIssue
	// UnreadableRepositories は照合対象を持たない読めない repository で、問題ではなく参考情報である。
	UnreadableRepositories []unreadableRepository
	// RefListFailures は recovery ref を読めなかった repository のうち、まだ必要とされている記録である。
	// 1 件の失敗で検査全体を止めないよう、repository 単位の問題として保持する。
	RefListFailures []unreadableRepository
	Errors          []string
}

// categories は従来の category ごとの文字列一覧へ畳み込む。
// reconcile の隔離記録と prune の対象選択はこの形を境界としており、doctor の分類はこの map に依存しない。
func (r artifactReport) categories() map[string]any {
	missingPaths := make([]string, 0, len(r.Missing))
	for _, item := range r.Missing {
		missingPaths = append(missingPaths, fmt.Sprintf("%s (%s, %s)", item.Path, item.SlotID, item.State))
	}
	unknownRefs, mismatchedRefs, missingRefs := refKeys(r.UnknownRefs), refKeys(r.MismatchedRefs), refKeys(r.MissingRefs)
	unknownPaths := append([]string{}, r.UnknownPaths...)
	diagnosticErrors := append([]string{}, r.Errors...)
	// ref を読めなかった repository は doctor では repository 単位の問題として出すが、
	// reconcile の記録と prune の結果では従来どおり errors に載せ、どの repository の話かを path で示す。
	for _, failure := range r.RefListFailures {
		diagnosticErrors = append(diagnosticErrors, refListFailureMessage(failure))
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

// refListFailureMessage は recovery ref を読めなかった repository 1 件の説明である。
// 読み手が対象の repository を path で特定できるようにする。
func refListFailureMessage(failure unreadableRepository) string {
	return fmt.Sprintf("list recovery refs for %s (%s): %s", failure.RepositoryID, failure.Path, failure.Cause)
}

func refKeys(issues []recoveryRefIssue) []string {
	out := make([]string, 0, len(issues))
	for _, issue := range issues {
		out = append(out, issue.key())
	}
	return out
}

// artifactDiagnostics は所有権の照合結果を category ごとの文字列一覧で返す。
func (m *Manager) artifactDiagnostics(ctx context.Context) map[string]any {
	return m.artifactOwnershipReport(ctx).categories()
}

// artifactOwnershipReport は登録済み slot・root 配下の実体・recovery ref を突き合わせる。
// 照合できなかった対象は Errors に残し、無関係と決めつけて正常扱いにしない。
func (m *Manager) artifactOwnershipReport(ctx context.Context) artifactReport {
	report := artifactReport{
		UnknownPaths: []string{}, Missing: []missingArtifact{},
		UnknownRefs: []recoveryRefIssue{}, MismatchedRefs: []recoveryRefIssue{}, MissingRefs: []recoveryRefIssue{},
		Errors: []string{},
	}
	artifacts, err := m.store.SlotArtifacts(ctx)
	if err != nil {
		report.Errors = append(report.Errors, err.Error())
		return report
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
			report.Errors = append(report.Errors, fmt.Sprintf("inspect slot %s: %v", artifact.ID, statErr))
		} else if !exists {
			report.Missing = append(report.Missing, missingArtifact{SlotID: artifact.ID, Path: clean, State: artifact.State})
		}
	}
	m.appendUnknownRootPaths(ctx, &report, expectedPaths)
	m.appendRecoveryRefIssues(ctx, &report)
	return report
}

func (m *Manager) appendUnknownRootPaths(ctx context.Context, report *artifactReport, expectedPaths map[string]state.SlotArtifact) {
	roots, rootsErr := m.rootPathsFromStore(ctx)
	if rootsErr != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("list worktree root generations: %v", rootsErr))
	}
	for _, root := range roots {
		paths, pathsErr := m.ownedRootArtifactPaths(root)
		if pathsErr != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("inspect root %s: %v", root, pathsErr))
			continue
		}
		for _, path := range paths {
			clean := filepath.Clean(path)
			if _, exists := expectedPaths[clean]; !exists {
				report.UnknownPaths = append(report.UnknownPaths, clean)
			}
		}
	}
}

func (m *Manager) appendRecoveryRefIssues(ctx context.Context, report *artifactReport) {
	repositories, err := m.store.Repositories(ctx)
	if err != nil {
		report.Errors = append(report.Errors, err.Error())
		return
	}
	// 所属の分からない repository は登録済みとして扱い、まだ使う予定のある記録の故障を参考情報へ落とさない。
	registered, registeredErr := m.store.RegisteredRepositoryIDs(ctx)
	if registeredErr != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("list registered repositories: %v", registeredErr))
		registered = nil
	}
	for _, repository := range repositories {
		expectedList, refsErr := m.store.RecoveryRefExpectations(ctx, string(repository.ID))
		if refsErr != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("read recovery refs for %s: %v", repository.ID, refsErr))
			continue
		}
		expected := map[string]state.RecoveryRefExpectation{}
		for _, ref := range expectedList {
			expected[ref.Ref] = ref
		}
		listed, listErr := m.git.Run(ctx, string(repository.MainPath), "for-each-ref", "--format=%(refname) %(objectname)", "refs/wx/recovery")
		if listErr != nil {
			// 登録済み workspace に属さず照合すべき snapshot も無い repository は、refs を読めなくても不明な点が残らない。
			// forget が記録を消さなかった頃の DB で、その path が Git リポジトリでなくなった場合に残る。
			// 回収は GC の PruneRepositories が行うため、待つ間の失敗を利用者への問題にしない。
			if len(expected) == 0 && registered != nil && !registered[string(repository.ID)] {
				report.UnreadableRepositories = append(report.UnreadableRepositories,
					unreadableRepository{RepositoryID: string(repository.ID), Path: string(repository.MainPath), Cause: listErr.Error()})
				continue
			}
			// 失敗はこの repository の照合だけを止める。残りの repository と他の検査は続ける。
			report.RefListFailures = append(report.RefListFailures,
				unreadableRepository{RepositoryID: string(repository.ID), Path: string(repository.MainPath), Cause: listErr.Error()})
			continue
		}
		actual := map[string]bool{}
		for _, line := range strings.Split(strings.TrimSpace(listed.Stdout), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			if len(fields) != 2 {
				report.Errors = append(report.Errors, fmt.Sprintf("parse recovery ref listing for %s: %q", repository.ID, line))
				continue
			}
			ref, oid := fields[0], fields[1]
			actual[ref] = true
			want, known := expected[ref]
			switch {
			case !known:
				report.UnknownRefs = append(report.UnknownRefs, recoveryRefIssue{RepositoryID: string(repository.ID), Ref: ref})
			case want.OID != oid:
				report.MismatchedRefs = append(report.MismatchedRefs, recoveryRefIssue{RepositoryID: string(repository.ID), Ref: ref, ExpiresAt: want.ExpiresAt})
			}
		}
		for ref, expectation := range expected {
			if !actual[ref] && !expectation.InFlight {
				report.MissingRefs = append(report.MissingRefs, recoveryRefIssue{RepositoryID: string(repository.ID), Ref: ref, ExpiresAt: expectation.ExpiresAt})
			}
		}
	}
}
