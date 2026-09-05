package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
)

const ownershipMarkerPrefix = ".wx-owner-"

// ownershipMarkerVersionはマーカースキーマです。バージョン2では、 絶対ターゲットパス：耐久性のあるルート生成と記録されたinode
// sQLiteのアイデンティティは、冗長パスが果たした役割を担い、 設定されたルートが移動したときにマーカーを書き換える必要がなくなりました。
const ownershipMarkerVersion = 2

type ownershipMarker struct {
	Version      int    `json:"version"`
	SlotID       string `json:"slot_id"`
	RootID       string `json:"root_id"`
	RepositoryID string `json:"repository_id"`
	CommonDir    string `json:"common_dir"`
}

// MarkerIdentityは、1つのリポジトリワークツリーのスロットスコープマーカーに名前を付けます。
// マーカーは、SQLiteが失われた場合の所有権の唯一のディスク上の証拠であるため、 スロットディレクトリ（ワークツリーの親）にあり、 ワークツリー自体の削除。
type MarkerIdentity struct {
	SlotID       string
	RootID       string
	RepositoryID string
}

func (m MarkerIdentity) validate(requireSlot bool) error {
	if m.RepositoryID == "" || strings.ContainsAny(m.RepositoryID, `/\`) || m.RepositoryID == "." || m.RepositoryID == ".." {
		return errors.New("invalid wx ownership repository id")
	}
	if m.RootID == "" || strings.ContainsAny(m.RootID, `/\`) {
		return errors.New("invalid wx ownership root id")
	}
	if strings.ContainsAny(m.SlotID, `/\`) || (requireSlot && m.SlotID == "") {
		return errors.New("invalid wx ownership slot id")
	}
	return nil
}

// EnsureOwnershipMarkerAtは、
// デーモンが保持しているワークツリールートがピン留めされています。マーカーの作成を同じ状態に保ちます
// 割り当てとワークツリーの準備としてのinode名前空間。
func EnsureOwnershipMarkerAt(owner *os.Root, root, target string, identity MarkerIdentity, commonDir string) error {
	if err := identity.validate(true); err != nil {
		return err
	}
	marker, err := newOwnershipMarkerAt(owner, root, target, identity, commonDir, true)
	if err != nil {
		return err
	}
	markerRelative, err := ownershipMarkerRelative(root, target, identity.RepositoryID)
	if err != nil {
		return err
	}
	return ensureOwnershipMarkerAt(owner, markerRelative, marker)
}

func ensureOwnershipMarkerAt(owner *os.Root, markerRelative string, marker ownershipMarker) error {
	if owner == nil {
		return errors.New("wx ownership root is nil")
	}
	if err := owner.MkdirAll(filepath.Dir(markerRelative), 0o700); err != nil {
		return fmt.Errorf("create wx ownership marker parent: %w", err)
	}
	if _, err := owner.Lstat(markerRelative); err == nil {
		return validateMarkerContents(owner, markerRelative, marker)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	data, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	file, err := owner.OpenFile(markerRelative, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return validateMarkerContents(owner, markerRelative, marker)
		}
		return fmt.Errorf("create wx ownership marker: %w", err)
	}
	if _, writeErr := file.Write(data); writeErr != nil {
		_ = file.Close()
		_ = owner.Remove(markerRelative)
		return fmt.Errorf("write wx ownership marker: %w", writeErr)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = owner.Remove(markerRelative)
		return fmt.Errorf("sync wx ownership marker: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = owner.Remove(markerRelative)
		return fmt.Errorf("close wx ownership marker: %w", err)
	}
	return validateMarkerContents(owner, markerRelative, marker)
}

// ValidateOwnershipMarkerAtは、以前にピン留めされた
// ルート記述子。パス名は、呼び出し元が
// この読み取りを外部ディレクトリにリダイレクトせずに記述子を使用します。
func ValidateOwnershipMarkerAt(owner *os.Root, root, target string, identity MarkerIdentity, commonDir string) error {
	if err := identity.validate(false); err != nil {
		return markerOwnershipFailure(err)
	}
	marker, err := newOwnershipMarkerAt(owner, root, target, identity, commonDir, false)
	if err != nil {
		return markerOwnershipFailure(err)
	}
	markerRelative, err := ownershipMarkerRelative(root, target, identity.RepositoryID)
	if err != nil {
		return markerOwnershipFailure(err)
	}
	actual, err := readOwnershipMarker(owner, markerRelative)
	if err != nil {
		return markerOwnershipFailure(err)
	}
	if actual.RootID != marker.RootID || actual.RepositoryID != marker.RepositoryID || actual.CommonDir != marker.CommonDir {
		return markerOwnershipFailure(errors.New("wx ownership marker does not match expected worktree"))
	}
	if identity.SlotID != "" && actual.SlotID != identity.SlotID {
		return markerOwnershipFailure(errors.New("wx ownership marker does not match expected slot"))
	}
	return nil
}

// ValidateRemovalOwnershipは、物理的な
// ワークツリーリーフが欠落しており、マーカーによってエンコードされたスロットIDを返します。
func ValidateRemovalOwnership(root, target string, identity MarkerIdentity, commonDir string) (string, error) {
	if err := identity.validate(false); err != nil {
		return "", markerOwnershipFailure(err)
	}
	marker, err := newOwnershipMarker(target, identity, commonDir, true)
	if err != nil {
		return "", markerOwnershipFailure(err)
	}
	owner, markerRelative, err := openMarkerRoot(root, target, identity.RepositoryID)
	if err != nil {
		return "", markerOwnershipFailure(err)
	}
	defer func() { _ = owner.Close() }()
	actual, err := readOwnershipMarker(owner, markerRelative)
	if err != nil {
		return "", markerOwnershipFailure(err)
	}
	if err := compareRemovalMarker(actual, marker); err != nil {
		return "", err
	}
	return actual.SlotID, nil
}

// ValidateRemovalOwnershipAtは、
// 構成されたwxルートがピン留めされている間のデーモン。ターゲットの解決を回避します
// 期待されるマーカーを構築しながら、ミュータブルな語彙ルートを通過します。
func ValidateRemovalOwnershipAt(owner *os.Root, root, target string, identity MarkerIdentity, commonDir string) (string, error) {
	if err := identity.validate(false); err != nil {
		return "", markerOwnershipFailure(err)
	}
	marker, err := newOwnershipMarkerAt(owner, root, target, identity, commonDir, true)
	if err != nil {
		return "", markerOwnershipFailure(err)
	}
	markerRelative, err := ownershipMarkerRelative(root, target, identity.RepositoryID)
	if err != nil {
		return "", markerOwnershipFailure(err)
	}
	actual, err := readOwnershipMarker(owner, markerRelative)
	if err != nil {
		return "", markerOwnershipFailure(err)
	}
	if err := compareRemovalMarker(actual, marker); err != nil {
		return "", err
	}
	return actual.SlotID, nil
}

func compareRemovalMarker(actual, expected ownershipMarker) error {
	if actual.RootID != expected.RootID || actual.RepositoryID != expected.RepositoryID {
		return markerOwnershipFailure(errors.New("wx ownership marker does not match recorded worktree"))
	}
	if actual.CommonDir != expected.CommonDir {
		return markerOwnershipFailure(errors.New("wx ownership marker common directory does not match recorded worktree"))
	}
	return nil
}

func markerOwnershipFailure(err error) error {
	if err == nil || errors.Is(err, state.ErrOwnership) {
		return err
	}
	return fmt.Errorf("%w: %w", state.ErrOwnership, err)
}

func newOwnershipMarker(target string, identity MarkerIdentity, commonDir string, allowMissingTarget bool) (ownershipMarker, error) {
	absoluteTarget, err := filepath.Abs(filepath.Clean(target))
	if err != nil {
		return ownershipMarker{}, err
	}
	if allowMissingTarget {
		if err := validatePhysicalPathAllowMissingLeaf(absoluteTarget); err != nil {
			return ownershipMarker{}, err
		}
	} else if err := domain.ValidatePhysicalPath(absoluteTarget, false); err != nil {
		return ownershipMarker{}, fmt.Errorf("worktree target is not physical: %w", err)
	}
	if info, statErr := os.Lstat(absoluteTarget); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return ownershipMarker{}, errors.New("worktree target is not a physical directory")
		}
	} else if !allowMissingTarget || !errors.Is(statErr, os.ErrNotExist) {
		return ownershipMarker{}, statErr
	}
	return markerExpectation(identity, commonDir)
}

// newOwnershipMarkerAtは、 ターゲットパス名。ターゲットは、最初に所有者を通じて到達可能であることが証明されます。
// （または、それが欠落しているリーフである場合に記述子セーフな親を持つこと）。 バージョン2では、マーカーは絶対パスをまったく記録しないため、ルートは
// それが運ぶ世代IDは、それを特定のwxルートに結びつけるものです。
func newOwnershipMarkerAt(owner *os.Root, root, target string, identity MarkerIdentity, commonDir string, allowMissingTarget bool) (ownershipMarker, error) {
	if owner == nil {
		return ownershipMarker{}, errors.New("wx ownership root is nil")
	}
	absoluteRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return ownershipMarker{}, err
	}
	absoluteTarget, err := filepath.Abs(filepath.Clean(target))
	if err != nil {
		return ownershipMarker{}, err
	}
	if !domain.IsWithin(absoluteRoot, absoluteTarget) {
		return ownershipMarker{}, errors.New("worktree target is outside wx ownership root")
	}
	relative, err := filepath.Rel(absoluteRoot, absoluteTarget)
	if err != nil {
		return ownershipMarker{}, err
	}
	info, statErr := owner.Lstat(relative)
	switch {
	case statErr == nil:
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return ownershipMarker{}, errors.New("worktree target is not a physical directory")
		}
		if directory, _, openErr := domain.OpenDirectoryAt(owner, relative); openErr != nil {
			return ownershipMarker{}, openErr
		} else if closeErr := directory.Close(); closeErr != nil {
			return ownershipMarker{}, closeErr
		}
	case !allowMissingTarget || !errors.Is(statErr, os.ErrNotExist):
		return ownershipMarker{}, statErr
	default:
		parent := filepath.Dir(relative)
		if directory, _, openErr := domain.OpenDirectoryAt(owner, parent); openErr != nil {
			return ownershipMarker{}, fmt.Errorf("worktree target parent is not physical: %w", openErr)
		} else if closeErr := directory.Close(); closeErr != nil {
			return ownershipMarker{}, closeErr
		}
	}
	return markerExpectation(identity, commonDir)
}

func markerExpectation(identity MarkerIdentity, commonDir string) (ownershipMarker, error) {
	absoluteCommon, err := filepath.Abs(filepath.Clean(commonDir))
	if err != nil {
		return ownershipMarker{}, err
	}
	absoluteCommon, err = filepath.EvalSymlinks(absoluteCommon)
	if err != nil {
		return ownershipMarker{}, fmt.Errorf("canonicalize Git common directory: %w", err)
	}
	return ownershipMarker{
		Version: ownershipMarkerVersion, SlotID: identity.SlotID, RootID: identity.RootID,
		RepositoryID: identity.RepositoryID, CommonDir: filepath.Clean(absoluteCommon),
	}, nil
}

func openMarkerRoot(root, target, repositoryID string) (*os.Root, string, error) {
	absoluteRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return nil, "", err
	}
	relative, err := ownershipMarkerRelative(absoluteRoot, target, repositoryID)
	if err != nil {
		return nil, "", err
	}
	owner, markerRelative, err := domain.OpenOwnedRoot(absoluteRoot, filepath.Join(absoluteRoot, relative))
	if err != nil {
		return nil, "", err
	}
	return owner, markerRelative, nil
}

// ownershipMarkerRelativeは、ワークツリーの親にマーカーを配置します。 wxレイアウトでは常にスロットディレクトリです。外に置いておく
// ワークツリーは、中断された削除が再び所有権を証明できるようにするものです。 retry:ワークツリーがなくなり、マーカーがなくなりました。
func ownershipMarkerRelative(root, target, repositoryID string) (string, error) {
	absoluteRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", err
	}
	absoluteTarget, err := filepath.Abs(filepath.Clean(target))
	if err != nil {
		return "", err
	}
	name, err := ownershipMarkerName(repositoryID)
	if err != nil {
		return "", err
	}
	marker := filepath.Join(filepath.Dir(absoluteTarget), name)
	if !domain.IsWithin(absoluteRoot, marker) {
		return "", errors.New("wx ownership marker is outside ownership root")
	}
	return filepath.Rel(absoluteRoot, marker)
}

func ownershipMarkerName(repositoryID string) (string, error) {
	if repositoryID == "" || strings.ContainsAny(repositoryID, `/\`) || repositoryID == "." || repositoryID == ".." {
		return "", errors.New("invalid wx ownership repository id")
	}
	return ownershipMarkerPrefix + repositoryID, nil
}

// OwnershipMarkerNameは、内部/デーモンで使用されるエクスポートされたスペルです
// マルチリポジトリワークスペースバンドルからスロットのマーカーを除外します。 マーカーは、そのバンドルのルートであるスロットディレクトリにあり、
// アーカイブとプレリストアプルーンの両方を保存する必要があります。
func OwnershipMarkerName(repositoryID string) string {
	return ownershipMarkerPrefix + repositoryID
}

func validateMarkerContents(owner *os.Root, relative string, expected ownershipMarker) error {
	actual, err := readOwnershipMarker(owner, relative)
	if err != nil {
		return err
	}
	if actual != expected {
		return errors.New("wx ownership marker does not match expected slot")
	}
	return nil
}

func readOwnershipMarker(owner *os.Root, relative string) (ownershipMarker, error) {
	info, err := owner.Lstat(relative)
	if err != nil {
		return ownershipMarker{}, fmt.Errorf("wx ownership marker is missing: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return ownershipMarker{}, errors.New("wx ownership marker is not an owner-only regular file")
	}
	data, err := owner.ReadFile(relative)
	if err != nil {
		return ownershipMarker{}, fmt.Errorf("read wx ownership marker: %w", err)
	}
	var marker ownershipMarker
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&marker); err != nil {
		return ownershipMarker{}, fmt.Errorf("decode wx ownership marker: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return ownershipMarker{}, errors.New("wx ownership marker has trailing data")
		}
		return ownershipMarker{}, fmt.Errorf("decode wx ownership marker trailing data: %w", err)
	}
	if marker.Version != ownershipMarkerVersion || marker.SlotID == "" || strings.ContainsAny(marker.SlotID, `/\`) || marker.RootID == "" || marker.RepositoryID == "" || marker.CommonDir == "" {
		return ownershipMarker{}, errors.New("wx ownership marker is incomplete")
	}
	return marker, nil
}

func validatePhysicalPathAllowMissingLeaf(path string) error {
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return err
	}
	if info, statErr := os.Lstat(absolute); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("worktree target is not a physical directory")
		}
		return domain.ValidatePhysicalPath(absolute, false)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	return domain.ValidatePhysicalPath(filepath.Dir(absolute), false)
}

// RegisteredWorktreeLockReasonは、ターゲットのGitロック理由を返します。
// 見つかった結果は、Gitがそのパスに登録していない場合にfalseです。
// 暫定的な除去後のケース。
func RegisteredWorktreeLockReason(ctx context.Context, runner *gitx.Runner, mainPath, target string) (reason string, found bool, err error) {
	reason, _, found, err = RegisteredWorktreeLockStatus(ctx, runner, mainPath, target)
	return reason, found, err
}

// RegisteredWorktreeLockStatusは、
// RegisteredWorktreeLockReason。ロック解除されたワークツリーと
// 空の理由でロックします。これは、ハンドオフを削除する際に重要です。
func RegisteredWorktreeLockStatus(ctx context.Context, runner *gitx.Runner, mainPath, target string) (reason string, locked, found bool, err error) {
	listed, err := runner.Run(ctx, mainPath, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return "", false, false, err
	}
	want, err := canonicalPathAllowMissing(target)
	if err != nil {
		return "", false, false, err
	}
	for _, record := range gitx.ParseWorktreeRecords(listed.Stdout) {
		if err := validatePhysicalPathAllowMissingLeaf(record.Path); err != nil {
			// シンボリックリンクエイリアスを介して到達したGit登録は、
			// 所有権の一致。最初に解決すると、パスの置換が非表示になります。
			continue
		}
		got, resolveErr := canonicalPathAllowMissing(record.Path)
		if resolveErr != nil || got != want {
			continue
		}
		return record.LockReason, record.Locked, true, nil
	}
	return "", false, false, nil
}

// ValidateRegisteredWorktreeAtは、inodeに対してGitの登録を検証します
// 記述子バインドされたターゲットによって保持されます。RegisteredWorktreeLockStatusとは異なり、
// ミュータブルなターゲットパス名を正規化することはないので、ルート置換は 外部ディレクトリを明らかなwxマッチに変換します。
func ValidateRegisteredWorktreeAt(ctx context.Context, runner *gitx.Runner, mainPath string, owner *os.Root, root, relativeTarget, targetIdentity string, slotID string, requireLock bool) error {
	reason, _, found, err := RegisteredWorktreeLockStatusAt(ctx, runner, mainPath, owner, root, relativeTarget, targetIdentity)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("worktree is not registered at its recorded path")
	}
	if !requireLock {
		return nil
	}
	if reason == "" {
		return errors.New("worktree is not protected by git worktree lock")
	}
	if slotID == "" {
		if !strings.HasPrefix(reason, "wx:") {
			return errors.New("worktree lock is not owned by wx")
		}
		return nil
	}
	if !domain.ValidWxLockReason(reason, slotID) {
		return fmt.Errorf("worktree lock reason does not belong to wx slot %s", slotID)
	}
	return nil
}

// RegisteredWorktreeLockStatusAtは、すべてのGit登録を
// 所有者を介して予想されるターゲットinode。ルート外の登録は無視されます
// シンボリックリンクエイリアスを介して解決するのではなく、
func RegisteredWorktreeLockStatusAt(ctx context.Context, runner *gitx.Runner, mainPath string, owner *os.Root, root, relativeTarget, targetIdentity string) (reason string, locked, found bool, err error) {
	if owner == nil {
		return "", false, false, errors.New("wx ownership root is nil")
	}
	if targetIdentity == "" {
		return "", false, false, errors.New("worktree target identity is unavailable")
	}
	listed, err := runner.Run(ctx, mainPath, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return "", false, false, err
	}
	absoluteRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", false, false, err
	}
	for _, record := range gitx.ParseWorktreeRecords(listed.Stdout) {
		absoluteRecord, absErr := filepath.Abs(filepath.Clean(record.Path))
		if absErr != nil || !domain.IsWithin(absoluteRoot, absoluteRecord) {
			continue
		}
		relativeRecord, relErr := filepath.Rel(absoluteRoot, absoluteRecord)
		if relErr != nil {
			continue
		}
		if filepath.Clean(relativeRecord) != filepath.Clean(relativeTarget) {
			continue
		}
		directory, identity, openErr := domain.OpenDirectoryAt(owner, relativeRecord)
		if openErr != nil {
			continue
		}
		closeErr := directory.Close()
		if closeErr != nil {
			return "", false, false, closeErr
		}
		if identity == targetIdentity {
			return record.LockReason, record.Locked, true, nil
		}
	}
	return "", false, false, nil
}

func canonicalPathAllowMissing(path string) (string, error) {
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	if resolved, evalErr := filepath.EvalSymlinks(absolute); evalErr == nil {
		return filepath.Clean(resolved), nil
	} else if !errors.Is(evalErr, os.ErrNotExist) {
		return "", evalErr
	}
	parent, err := canonicalPathAllowMissing(filepath.Dir(absolute))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(absolute)), nil
}
