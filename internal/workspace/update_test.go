package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
	"github.com/HappyOnigiri/WorktreeX/internal/state"
)

type updateOwnershipValidatorFunc func(context.Context, state.WorktreeOwnershipRequest) (state.WorktreeOwnership, error)

func (f updateOwnershipValidatorFunc) ValidateWorktreeOwnership(ctx context.Context, request state.WorktreeOwnershipRequest) (state.WorktreeOwnership, error) {
	return f(ctx, request)
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestValidateAndSyncRootPlacementsUpdatesRecordedCopyAndPreservesGeneratedFile(t *testing.T) {
	t.Parallel()
	source, target := t.TempDir(), t.TempDir()
	sourcePath := filepath.Join(source, "config", "local.cfg")
	if err := os.MkdirAll(filepath.Dir(sourcePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(target, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, []byte("new\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sourcePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "config", "local.cfg"), []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	generated := filepath.Join(target, "config", "generated.log")
	if err := os.WriteFile(generated, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hash := func(value string) string {
		sum := sha256.Sum256([]byte(value))
		return hex.EncodeToString(sum[:])
	}
	previous := []state.Placement{{RelativePath: "config/local.cfg", Kind: "copy", SourcePath: sourcePath, ContentSHA256: hash("old\n")}}
	desired := []state.Placement{{RelativePath: "config/local.cfg", Kind: "copy", SourcePath: sourcePath, ContentSHA256: hash("new\n")}}
	root, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := ValidateAndSyncRootPlacements(root, previous, desired); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(target, "config", "local.cfg")); err != nil || string(got) != "new\n" {
		t.Fatalf("updated copy=%q err=%v", got, err)
	}
	if info, err := os.Stat(filepath.Join(target, "config", "local.cfg")); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("updated copy mode=%o err=%v, want 700", info.Mode().Perm(), err)
	}
	if got, err := os.ReadFile(generated); err != nil || string(got) != "keep\n" {
		t.Fatalf("generated file=%q err=%v", got, err)
	}
}

// 内容が同じでも root copy の permission mode は新規準備と同じ状態へ同期する。
func TestValidateAndSyncRootPlacementsUpdatesModeOnly(t *testing.T) {
	t.Parallel()
	source, target := t.TempDir(), t.TempDir()
	sourcePath := filepath.Join(source, "scripts", "check.sh")
	targetPath := filepath.Join(target, "scripts", "check.sh")
	if err := os.MkdirAll(filepath.Dir(sourcePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o700); err != nil {
		t.Fatal(err)
	}
	content := []byte("#!/bin/sh\nexit 0\n")
	if err := os.WriteFile(sourcePath, content, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sourcePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(targetPath, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	placement := state.Placement{
		RelativePath:  "scripts/check.sh",
		Kind:          "copy",
		SourcePath:    sourcePath,
		ContentSHA256: hex.EncodeToString(sum[:]),
	}
	root, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := ValidateAndSyncRootPlacements(root, []state.Placement{placement}, []state.Placement{placement}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("mode=%o, want 700 after mode-only update", got)
	}
	if got, err := os.ReadFile(targetPath); err != nil || string(got) != string(content) {
		t.Fatalf("content=%q err=%v, want unchanged content", got, err)
	}
}

func TestValidateAndSyncRootPlacementsReplacesRecordedDirectoryWithFile(t *testing.T) {
	t.Parallel()
	source, target := t.TempDir(), t.TempDir()
	sourcePath := filepath.Join(source, "config")
	if err := os.WriteFile(sourcePath, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(target, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "config", "old.cfg"), []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hash := func(value string) string {
		sum := sha256.Sum256([]byte(value))
		return hex.EncodeToString(sum[:])
	}
	previous := []state.Placement{{RelativePath: "config/old.cfg", Kind: "copy", SourcePath: filepath.Join(source, "old.cfg"), ContentSHA256: hash("old\n")}}
	desired := []state.Placement{{RelativePath: "config", Kind: "copy", SourcePath: sourcePath, ContentSHA256: hash("new\n")}}
	root, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := ValidateAndSyncRootPlacements(root, previous, desired); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(target, "config")); err != nil || string(got) != "new\n" {
		t.Fatalf("replacement=%q err=%v", got, err)
	}
}

// 同じ link は copy 用の mode 同期へ回さず、既存の symlink をそのまま再利用する。
func TestValidateAndSyncRootPlacementsKeepsUnchangedLink(t *testing.T) {
	t.Parallel()
	source := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(source, []byte("source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := os.Symlink(source, filepath.Join(target, "linked.txt")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	placement := state.Placement{RelativePath: "linked.txt", Kind: "link", SourcePath: source}
	if err := ValidateAndSyncRootPlacements(root, []state.Placement{placement}, []state.Placement{placement}); err != nil {
		t.Fatalf("unchanged link: %v", err)
	}
	got, err := root.Readlink("linked.txt")
	if err != nil || got != source {
		t.Fatalf("link target=%q err=%v, want %q", got, err, source)
	}
}

// 既存配置の欠落は、要求された配置が空でも更新不適格として返す。
func TestValidateRootPlacementsRejectsMissingRecordedPlacement(t *testing.T) {
	t.Parallel()
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	previous := []state.Placement{{RelativePath: "missing", Kind: "copy", ContentSHA256: "hash"}}
	if err := ValidateRootPlacements(root, previous, nil); err == nil {
		t.Fatal("missing recorded placement was accepted")
	}
}

// 配置済みディレクトリの中に未記録 file が残る場合は、再利用を拒否する。
func TestDirectoryCoveredByPlacementsRejectsWalkError(t *testing.T) {
	t.Parallel()
	target := t.TempDir()
	if err := os.Mkdir(filepath.Join(target, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(target, "config", "blocked")
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "value"), []byte("value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })
	root, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if _, err := directoryCoveredByPlacements(root, "config", nil); err == nil {
		t.Fatal("walk permission error was ignored")
	}
}

// 配置先ディレクトリに未記録 file があれば、そのディレクトリ全体を既存配置だけの実体とは扱わない。
func TestDirectoryCoveredByPlacementsRejectsUnrecordedFile(t *testing.T) {
	t.Parallel()
	target := t.TempDir()
	if err := os.Mkdir(filepath.Join(target, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "config", "generated"), []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	covered, err := directoryCoveredByPlacements(root, "config", nil)
	if err != nil {
		t.Fatal(err)
	}
	if covered {
		t.Fatal("directory with an unrecorded file was accepted as covered")
	}
}

// 既存配置と同じ copy の mode 同期に失敗した場合は、後段の内容検査が通っても更新を成功させない。
func TestMaterializeChangedPlacementsPropagatesCopyModeError(t *testing.T) {
	t.Parallel()
	target := t.TempDir()
	content := []byte("recorded\n")
	if err := os.WriteFile(filepath.Join(target, "config"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	placement := state.Placement{
		RelativePath: "config", SourcePath: filepath.Join(t.TempDir(), "missing"),
		Kind: "copy", ContentSHA256: hex.EncodeToString(sum[:]),
	}
	root, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := materializeChangedPlacements(root, []state.Placement{placement}, []state.Placement{placement}); err == nil {
		t.Fatal("copy mode synchronization error was ignored")
	}
}

// copy が配置先の形状を拒否した場合は、後段の配置検査へ置き換えず元の失敗を返す。
func TestMaterializeChangedPlacementsPropagatesCopyError(t *testing.T) {
	t.Parallel()
	source := filepath.Join(t.TempDir(), "source")
	content := []byte("source\n")
	if err := os.WriteFile(source, content, 0o600); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := os.Symlink(source, filepath.Join(target, "copy")); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	placement := state.Placement{
		RelativePath: "copy", SourcePath: source, Kind: "copy",
		ContentSHA256: hex.EncodeToString(sum[:]),
	}
	root, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	err = materializeChangedPlacements(root, nil, []state.Placement{placement})
	if err == nil || !strings.Contains(err.Error(), "copy target copy is not a regular file") {
		t.Fatalf("copy error=%v, want the materialization failure", err)
	}
}

// 未記録の実体が要求配置先にあれば、既存 file でも上書き可能な配置とは扱わない。
func TestValidateRootPlacementsRejectsUnrecordedCollision(t *testing.T) {
	t.Parallel()
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "occupied"), []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	desired := []state.Placement{{RelativePath: "occupied", Kind: "link", SourcePath: filepath.Join(t.TempDir(), "source")}}
	err = ValidateRootPlacements(root, nil, desired)
	if !errors.Is(err, ErrUpdateIneligible) {
		t.Fatalf("unrecorded collision error=%v, want ErrUpdateIneligible", err)
	}
}

// 既存配置の親ディレクトリを置き換える検査で走査に失敗した場合は、安全な衝突なしとは扱わない。
func TestValidateRootPlacementsPropagatesDirectoryWalkError(t *testing.T) {
	t.Parallel()
	target := t.TempDir()
	configDir := filepath.Join(target, "config")
	if err := os.Mkdir(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	content := []byte("recorded\n")
	if err := os.WriteFile(filepath.Join(configDir, "z-recorded"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(configDir, "a-blocked")
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "value"), []byte("value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })
	sum := sha256.Sum256(content)
	previous := []state.Placement{{RelativePath: "config/z-recorded", Kind: "copy", ContentSHA256: hex.EncodeToString(sum[:])}}
	desired := []state.Placement{{RelativePath: "config", Kind: "copy"}}
	root, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := ValidateRootPlacements(root, previous, desired); err == nil {
		t.Fatal("directory walk error was ignored")
	}
}

// gitlink が同一な更新は submodule の実体を残したまま通り、gitlink が変わる更新は不適格として弾かれる。
// 更新経路は `checkout --detach --force` だけで submodule を触らないため、この2つが成り立つことが前提になる。
func TestUpdateKeepsMaterializedSubmoduleAndRejectsChangedGitlinks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSubmoduleFixture(t)
	if err := f.preparer.Prepare(ctx, f.repo, f.target, f.head, testSlotID); err != nil {
		t.Fatal(err)
	}
	gitlink := submoduleGitlink(t, f.repository, f.head)
	writeTestFile(t, filepath.Join(f.repository, "tracked"), "updated\n")
	gitCommand(t, f.repository, "add", "tracked")
	gitCommand(t, f.repository, "commit", "-m", "unrelated change")
	sameGitlink := gitOutput(t, f.repository, "rev-parse", "HEAD")
	if err := validateCollisionCandidate(ctx, f.preparer, f.repo, f.target, f.head, sameGitlink, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.preparer.UpdateLocked(ctx, f.repo, f.target, f.head, sameGitlink, testSlotID, nil, nil); err != nil {
		t.Fatal(err)
	}
	if head := gitOutput(t, f.submoduleTarget(), "rev-parse", "HEAD"); head != gitlink {
		t.Fatalf("submodule HEAD=%s after update, want the unchanged gitlink %s", head, gitlink)
	}
	if status := gitOutput(t, f.target, "status", "--porcelain", "--ignore-submodules=none"); status != "" {
		t.Fatalf("updated worktree status=%q, want clean", status)
	}
	// child を進めて gitlink を差し替えた OID は、submodule を再同期できないため更新に使えない。
	writeTestFile(t, filepath.Join(f.child, "kid.txt"), "ahead\n")
	gitCommand(t, f.child, "add", ".")
	gitCommand(t, f.child, "commit", "-m", "child ahead")
	ahead := gitOutput(t, f.child, "rev-parse", "HEAD")
	gitCommand(t, f.repository, "update-index", "--cacheinfo", "160000,"+ahead+",sub/kid")
	gitCommand(t, f.repository, "commit", "-m", "advance gitlink")
	changedGitlink := gitOutput(t, f.repository, "rev-parse", "HEAD")
	err := validateCollisionCandidate(ctx, f.preparer, f.repo, f.target, sameGitlink, changedGitlink, nil, nil)
	if !errors.Is(err, ErrUpdateIneligible) {
		t.Fatalf("changed gitlink update error=%v, want ErrUpdateIneligible", err)
	}
}

// 更新中の worktree が正常なら、既存検査を通過して detached HEAD の更新を許可する。
// testlint:allow-serial -- fixture preparation changes HOME through the shared setup
func TestValidateUpdatingAcceptsDetachedCleanWorktree(t *testing.T) {
	ctx := context.Background()
	f := newSubmoduleFixture(t)
	if err := f.preparer.Prepare(ctx, f.repo, f.target, f.head, testSlotID); err != nil {
		t.Fatal(err)
	}
	identity, err := f.preparer.WorktreeIdentity(f.target)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.preparer.validateUpdating(ctx, f.repo, f.target, f.head, testSlotID, identity); err != nil {
		t.Fatalf("valid detached worktree: %v", err)
	}
}

// 更新前提の worktree 検査に失敗した場合は、後続の状態検査が正常でも更新を許可しない。
// testlint:allow-serial -- cowFixture が隔離 repository の構築中に HOME を変更する。
func TestValidateUpdatingPropagatesWorktreeValidationError(t *testing.T) {
	ctx := context.Background()
	p, repo, oid, target := cowFixture(t)
	if err := p.Prepare(ctx, repo, target, oid, testSlotID); err != nil {
		t.Fatal(err)
	}
	identity, err := p.WorktreeIdentity(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.validateUpdating(ctx, repo, target, strings.Repeat("0", 40), testSlotID, identity); err == nil {
		t.Fatal("worktree validation error was ignored")
	}
}

// 更新中の state 所有権を証明できない場合は、clean な detached worktree でも更新を許可しない。
// testlint:allow-serial -- cowFixture が隔離 repository の構築中に HOME を変更する。
func TestValidateUpdatingPropagatesStateOwnershipError(t *testing.T) {
	ctx := context.Background()
	p, repo, oid, target := cowFixture(t)
	if err := p.Prepare(ctx, repo, target, oid, testSlotID); err != nil {
		t.Fatal(err)
	}
	p.Ownership = nil
	identity, err := p.WorktreeIdentity(target)
	if err != nil {
		t.Fatal(err)
	}
	err = p.validateUpdating(ctx, repo, target, oid, testSlotID, identity)
	if !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("state ownership error=%v, want state.ErrOwnership", err)
	}
}

// 更新中に tracked file が変わった場合は、detached HEAD の確認が通っても dirty な worktree を許可しない。
// testlint:allow-serial -- cowFixture が隔離 repository の構築中に HOME を変更する。
func TestValidateUpdatingPropagatesTrackedCleanError(t *testing.T) {
	ctx := context.Background()
	p, repo, oid, target := cowFixture(t)
	if err := p.Prepare(ctx, repo, target, oid, testSlotID); err != nil {
		t.Fatal(err)
	}
	identity, err := p.WorktreeIdentity(target)
	if err != nil {
		t.Fatal(err)
	}
	ownershipChecks := 0
	p.Ownership = updateOwnershipValidatorFunc(func(context.Context, state.WorktreeOwnershipRequest) (state.WorktreeOwnership, error) {
		ownershipChecks++
		if err := os.WriteFile(filepath.Join(target, "file"), []byte("dirty\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return state.WorktreeOwnership{}, nil
	})
	err = p.validateUpdating(ctx, repo, target, oid, testSlotID, identity)
	if !errors.Is(err, ErrTrackedChanges) {
		t.Fatalf("tracked clean error=%v, want ErrTrackedChanges", err)
	}
	if ownershipChecks != 1 {
		t.Fatalf("ownership checks=%d, want one state validation", ownershipChecks)
	}
}

// flag 付き path の新しい mode が 100644 と 100755 のどちらでも flag を張り直せるが、symlink へ変わる更新は弾く。
// testlint:allow-serial -- cowFixture が隔離 repository の構築中に HOME を変更する。
func TestValidateUpdateCandidateReinstatesFlagsOnlyOnRegularModes(t *testing.T) {
	ctx := context.Background()
	p, repo, baseOID, target := cowFixture(t)
	main := string(repo.MainPath)
	if err := p.Prepare(ctx, repo, target, baseOID, testSlotID); err != nil {
		t.Fatal(err)
	}
	cowGit(t, target, "update-index", "--skip-worktree", "file")
	if err := os.Chmod(filepath.Join(main, "file"), 0o755); err != nil {
		t.Fatal(err)
	}
	cowGit(t, main, "commit", "-q", "-am", "make the flagged file executable")
	executableOID := cowGit(t, main, "rev-parse", "HEAD")
	if err := validateCollisionCandidate(ctx, p, repo, target, baseOID, executableOID, nil, nil); err != nil {
		t.Fatalf("executable mode must stay restorable: %v", err)
	}
	if err := os.Remove(filepath.Join(main, "file")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere", filepath.Join(main, "file")); err != nil {
		t.Fatal(err)
	}
	cowGit(t, main, "add", "file")
	cowGit(t, main, "commit", "-q", "-m", "turn the flagged file into a symlink")
	symlinkOID := cowGit(t, main, "rev-parse", "HEAD")
	err := validateCollisionCandidate(ctx, p, repo, target, baseOID, symlinkOID, nil, nil)
	if !errors.Is(err, ErrUpdateIneligible) || !strings.Contains(err.Error(), "no regular file") {
		t.Fatalf("symlink at the flagged path: error=%v, want ErrUpdateIneligible", err)
	}
}

func updatePhaseCounts(timings *PhaseTimings) map[string]int {
	counts := map[string]int{}
	for _, phase := range timings.Phases() {
		counts[phase.Name] = phase.Count
	}
	return counts
}

func updateTestInode(t *testing.T, path string) uint64 {
	t.Helper()
	var info unix.Stat_t
	if err := unix.Stat(path, &info); err != nil {
		t.Fatal(err)
	}
	return info.Ino
}

// UPDATE の compaction は、その更新が書き直した path だけを候補にする。
// index 全体を候補に戻すと、前回の準備で共有済みのファイルへ置換経路を通し直し、所要時間が worktree の規模で決まる。
// testlint:allow-serial -- プロセス全体の環境（HOME）を変更するため
func TestUpdateLimitsCOWCompactionToRewrittenPaths(t *testing.T) {
	ctx := context.Background()
	p, repo, _, target := cowFixture(t)
	main := string(repo.MainPath)
	if err := os.WriteFile(filepath.Join(main, "kept"), []byte(cowBody+"kept\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(main, "changed"), []byte(cowBody+"before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cowGit(t, main, "add", ".")
	cowGit(t, main, "commit", "-m", "donors")
	baseOID := cowGit(t, main, "rev-parse", "HEAD")
	p.Config.Storage.CopyMode = config.CopyModeAuto
	if err := p.Prepare(ctx, repo, target, baseOID, testSlotID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(main, "changed"), []byte(cowBody+"after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cowGit(t, main, "add", ".")
	cowGit(t, main, "commit", "-m", "rewrite one tracked file")
	newOID := cowGit(t, main, "rev-parse", "HEAD")
	scope, err := p.updateCOWScope(ctx, repo, baseOID, newOID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(scope.rewritten) != 1 || !scope.rewritten["changed"] {
		t.Fatalf("scope=%v, want only the rewritten path", scope.rewritten)
	}
	before := updateTestInode(t, filepath.Join(target, "kept"))
	p.Phases = &PhaseTimings{}
	if _, err := p.UpdateLocked(ctx, repo, target, baseOID, newOID, testSlotID, nil, nil); err != nil {
		t.Fatal(err)
	}
	counts := updatePhaseCounts(p.Phases)
	if cowAvailable() && counts["cow.candidates"] != 1 {
		t.Fatalf("cow.candidates=%d, want the single rewritten path", counts["cow.candidates"])
	}
	if after := updateTestInode(t, filepath.Join(target, "kept")); after != before {
		t.Fatalf("an untouched path went through the replacement path again: %d -> %d", before, after)
	}
	if data, err := os.ReadFile(filepath.Join(target, "changed")); err != nil || string(data) != cowBody+"after\n" {
		t.Fatalf("updated bytes=%d %v", len(data), err)
	}
}

// include の配置だけが変わる更新は tracked file を1件も書き直さないため、置換経路へ入らない。
// 実測ではこの形が最も遅く、共有対象すべてに compare から unlink までを通し直していた。
// testlint:allow-serial -- プロセス全体の環境（HOME）を変更するため
func TestUpdateWithoutRewrittenTrackedPathsSkipsCOWReplacement(t *testing.T) {
	ctx := context.Background()
	p, repo, oid, target := cowFixture(t)
	main := string(repo.MainPath)
	p.Config.Storage.CopyMode = config.CopyModeAuto
	if err := p.Prepare(ctx, repo, target, oid, testSlotID); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(main, ".env.local")
	if err := os.WriteFile(source, []byte("included\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("included\n"))
	desired := []state.Placement{{RelativePath: ".env.local", Kind: "copy", SourcePath: source, ContentSHA256: hex.EncodeToString(sum[:])}}
	before := updateTestInode(t, filepath.Join(target, "file"))
	p.Phases = &PhaseTimings{}
	materialized, err := p.UpdateLocked(ctx, repo, target, oid, oid, testSlotID, nil, desired)
	if err != nil {
		t.Fatal(err)
	}
	if len(materialized) != 1 {
		t.Fatalf("materialized=%v", materialized)
	}
	counts := updatePhaseCounts(p.Phases)
	if cowAvailable() && counts["cow.entries"] == 0 {
		t.Fatal("compaction never ran, so the zero replacement counts prove nothing")
	}
	for _, name := range []string{"cow.candidates", "cow.compare", "cow.clone", "cow.metadata", "cow.swap", "cow.verify", "cow.unlink"} {
		if counts[name] != 0 {
			t.Fatalf("%s=%d after an update that rewrote no tracked file", name, counts[name])
		}
	}
	if after := updateTestInode(t, filepath.Join(target, "file")); after != before {
		t.Fatalf("a shared file went through the replacement path again: %d -> %d", before, after)
	}
}
