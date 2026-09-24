package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
	"github.com/HappyOnigiri/WorktreeX/internal/domain"
	"github.com/HappyOnigiri/WorktreeX/internal/state"
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

// submodule capsule ref の照合結果の種別である。削除の識別子体系は広げないため、報告の中でだけ使う。
const (
	submoduleRefUnknown    = "unknown"
	submoduleRefMismatched = "mismatched"
	submoduleRefMissing    = "missing"
)

// submoduleRefIssue は子の capsule ref 1 件の不整合である。
// 公開先が repository ごとの ref store ではないため、対象は ref 名ではなくローカル module の path で示す。
// ExpiresAt は ref を支える親 snapshot の期限で、unknown ref（DB に記録が無い）では空になる。
type submoduleRefIssue struct {
	Kind, ModuleDir, Path, Ref, ExpiresAt string
}

// 所有権の照合を完了できなかった原因の種別である。doctor がこの値で対処を分けるので、
// 「どこを見て何を実行するか」が同じ失敗だけを同じ種別にまとめる。
const (
	// ownershipFailureStore は state database への問い合わせが失敗した記録である。
	ownershipFailureStore = "store"
	// ownershipFailureSlotPath は登録済み slot の実体を確かめられなかった記録である。
	ownershipFailureSlotPath = "slot_path"
	// ownershipFailureRootPath は root 世代の中身を列挙できなかった記録である。
	ownershipFailureRootPath = "root_path"
	// ownershipFailureRepositoryRef は repository の recovery ref を読めなかった・解釈できなかった記録である。
	ownershipFailureRepositoryRef = "repository_ref"
)

// ownershipFailure は所有権の照合を完了できなかった 1 件である。
// Message は reconcile の記録と `wx prune` の結果へそのまま載る本文で、
// Kind と Target は doctor が対処と対象を分けるためだけに持つ。
type ownershipFailure struct{ Kind, Target, Message string }

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
	// SubmoduleRefIssues は子の capsule ref の不整合である。ref store が repository ごとではないため、
	// reconcile・prune が使う categories には載せず、doctor の報告だけで扱う。
	SubmoduleRefIssues []submoduleRefIssue
	// Errors は照合を完了できなかった記録である。種別を落とさずに持ち、doctor が原因ごとに対処を出せるようにする。
	Errors []ownershipFailure
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
	diagnosticErrors := make([]string, 0, len(r.Errors)+len(r.RefListFailures))
	for _, failure := range r.Errors {
		diagnosticErrors = append(diagnosticErrors, failure.Message)
	}
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
		Errors: []ownershipFailure{},
	}
	artifacts, err := m.store.SlotArtifacts(ctx)
	if err != nil {
		report.Errors = append(report.Errors, ownershipFailure{Kind: ownershipFailureStore, Message: err.Error()})
		return report
	}
	expectedPaths := expectedSlotPaths(artifacts)
	for _, artifact := range artifacts {
		clean := filepath.Clean(artifact.Path)
		if artifact.State == "ARCHIVED" || artifact.State == "REMOVING" {
			continue
		}
		exists, statErr := m.ownedPathExists(clean)
		if statErr != nil {
			report.Errors = append(report.Errors, ownershipFailure{
				Kind: ownershipFailureSlotPath, Target: clean,
				Message: fmt.Sprintf("inspect slot %s: %v", artifact.ID, statErr),
			})
		} else if !exists {
			report.Missing = append(report.Missing, missingArtifact{SlotID: artifact.ID, Path: clean, State: artifact.State})
		}
	}
	m.appendUnknownRootPaths(ctx, &report, expectedPaths)
	m.appendRecoveryRefIssues(ctx, &report)
	return report
}

// expectedSlotPaths は DB が説明する slot 実体の絶対 path 集合を返す。
// state で絞らないのは、回収途中（実体は消したが ARCHIVED の記録前）の登録を登録外と読み替えないためである。
// doctor の照合と登録外実体の列挙が同じ集合を見るよう、判定の出所をここ 1 か所にする。
func expectedSlotPaths(artifacts []state.SlotArtifact) map[string]state.SlotArtifact {
	expected := make(map[string]state.SlotArtifact, len(artifacts))
	for _, artifact := range artifacts {
		expected[filepath.Clean(artifact.Path)] = artifact
	}
	return expected
}

func (m *Manager) appendUnknownRootPaths(ctx context.Context, report *artifactReport, expectedPaths map[string]state.SlotArtifact) {
	roots, rootsErr := m.rootPathsFromStore(ctx)
	if rootsErr != nil {
		report.Errors = append(report.Errors, ownershipFailure{
			Kind: ownershipFailureStore, Message: fmt.Sprintf("list worktree root generations: %v", rootsErr),
		})
	}
	for _, root := range roots {
		paths, pathsErr := m.ownedRootArtifactPaths(root)
		if pathsErr != nil {
			report.Errors = append(report.Errors, ownershipFailure{
				Kind: ownershipFailureRootPath, Target: root,
				Message: fmt.Sprintf("inspect root %s: %v", root, pathsErr),
			})
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
		report.Errors = append(report.Errors, ownershipFailure{Kind: ownershipFailureStore, Message: err.Error()})
		return
	}
	// 所属の分からない repository は登録済みとして扱い、まだ使う予定のある記録の故障を参考情報へ落とさない。
	registered, registeredErr := m.store.RegisteredRepositoryIDs(ctx)
	if registeredErr != nil {
		report.Errors = append(report.Errors, ownershipFailure{
			Kind: ownershipFailureStore, Message: fmt.Sprintf("list registered repositories: %v", registeredErr),
		})
		registered = nil
	}
	for _, repository := range repositories {
		expectedList, refsErr := m.store.RecoveryRefExpectations(ctx, string(repository.ID))
		if refsErr != nil {
			report.Errors = append(report.Errors, ownershipFailure{
				Kind: ownershipFailureStore, Target: string(repository.MainPath),
				Message: fmt.Sprintf("read recovery refs for %s: %v", repository.ID, refsErr),
			})
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
				report.Errors = append(report.Errors, ownershipFailure{
					Kind: ownershipFailureRepositoryRef, Target: string(repository.MainPath),
					Message: fmt.Sprintf("parse recovery ref listing for %s: %q", repository.ID, line),
				})
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
		m.appendSubmoduleRefIssues(ctx, report, repository)
	}
}

// appendSubmoduleRefIssues は子の capsule ref を、それぞれの source のローカル module で照合する。
// 対象は submodule_snapshots の行を持つ module だけで、行の無い module は走査しない。
// `wx prune` の識別子は単一の ref store を前提にした `<repository_id>:<ref>` なので、ここでは報告だけを行い削除の対象にしない。
func (m *Manager) appendSubmoduleRefIssues(ctx context.Context, report *artifactReport, repository discovery.Repository) {
	expectations, err := m.store.SubmoduleRecoveryRefExpectations(ctx, string(repository.ID))
	if err != nil {
		report.Errors = append(report.Errors, ownershipFailure{
			Kind: ownershipFailureStore, Target: string(repository.MainPath),
			Message: fmt.Sprintf("read submodule recovery refs for %s: %v", repository.ID, err),
		})
		return
	}
	if len(expectations) == 0 {
		return
	}
	commonModules := filepath.Join(string(repository.CommonDir), "modules")
	expected := map[string]map[string]state.SubmoduleRecoveryRef{}
	for _, expectation := range expectations {
		if expected[expectation.Name] == nil {
			expected[expectation.Name] = map[string]state.SubmoduleRecoveryRef{}
		}
		expected[expectation.Name][expectation.Ref] = expectation
	}
	names := make([]string, 0, len(expected))
	for name := range expected {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		moduleDir := filepath.Join(commonModules, name)
		if !domain.IsWithin(commonModules, moduleDir) {
			report.Errors = append(report.Errors, ownershipFailure{
				Kind: ownershipFailureRepositoryRef, Target: moduleDir,
				Message: fmt.Sprintf("submodule %s of %s resolves outside the module directory", name, repository.ID),
			})
			continue
		}
		listed, listErr := m.git.Run(ctx, moduleDir, "--git-dir=.", "for-each-ref", "--format=%(refname) %(objectname)", "refs/wx/recovery")
		if listErr != nil {
			// module ごと読めない場合、その module の子は復元できない。欠落と同じ重さで報告する。
			for _, expectation := range expected[name] {
				report.SubmoduleRefIssues = append(report.SubmoduleRefIssues, submoduleRefIssue{Kind: submoduleRefMissing, ModuleDir: moduleDir, Path: expectation.Path, Ref: expectation.Ref, ExpiresAt: expectation.ExpiresAt})
			}
			continue
		}
		actual := map[string]bool{}
		for _, line := range strings.Split(strings.TrimSpace(listed.Stdout), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				continue
			}
			ref, oid := fields[0], fields[1]
			actual[ref] = true
			want, known := expected[name][ref]
			switch {
			case !known:
				report.SubmoduleRefIssues = append(report.SubmoduleRefIssues, submoduleRefIssue{Kind: submoduleRefUnknown, ModuleDir: moduleDir, Ref: ref})
			case want.OID != oid:
				report.SubmoduleRefIssues = append(report.SubmoduleRefIssues, submoduleRefIssue{Kind: submoduleRefMismatched, ModuleDir: moduleDir, Path: want.Path, Ref: ref, ExpiresAt: want.ExpiresAt})
			}
		}
		for ref, expectation := range expected[name] {
			if !actual[ref] {
				report.SubmoduleRefIssues = append(report.SubmoduleRefIssues, submoduleRefIssue{Kind: submoduleRefMissing, ModuleDir: moduleDir, Path: expectation.Path, Ref: ref, ExpiresAt: expectation.ExpiresAt})
			}
		}
	}
}
