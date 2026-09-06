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

// ownershipMarkerVersion は marker の schema 版である。version 2 の marker は絶対 target path を記録しない。
// 代わりに durable な root 世代 ID と SQLite が記録する inode identity がその役割を担うため、設定した root が移動しても marker を書き換えずに済む。
const ownershipMarkerVersion = 2

type ownershipMarker struct {
	Version      int    `json:"version"`
	SlotID       string `json:"slot_id"`
	RootID       string `json:"root_id"`
	RepositoryID string `json:"repository_id"`
	CommonDir    string `json:"common_dir"`
}

// MarkerIdentity は、1つの repository worktree に対応する slot 単位の marker を指定する。
// marker は SQLite を失ったときに残る唯一のディスク上の所有権の証拠なので、worktree 自身ではなくその親である slot ディレクトリに置き、worktree を消した後も残す。
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

// EnsureOwnershipMarkerAt は、daemon が worktree root を pin したまま marker を作る。
// marker の作成を、allocation と worktree 準備が使うのと同じ inode namespace に閉じ込めるためである。
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

// ValidateOwnershipMarkerAt は、pin 済みの root descriptor 経由で marker を検証する。
// path 名を開き直さないため、この読み取りが別ディレクトリへすり替わることがない。
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

// ValidateRemovalOwnership は、worktree の leaf が実体として無いときでも所有権を検証し、marker が記録する slot ID を返す。
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

// ValidateRemovalOwnershipAt は、daemon が設定済みの wx root を pin したまま使う版である。
// 期待する marker を組み立てる間、可変な path 名を辿って target を解決しない。
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

// newOwnershipMarkerAt は、target が owner 経由で到達できること（leaf が無いときは descriptor で安全に開ける親を持つこと）を確かめてから、期待する marker を組み立てる。
// version 2 の marker は絶対 path を一切記録しないため、marker を特定の wx root に結び付けるのは、そこに載る root 世代 ID である。
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

// ownershipMarkerRelative は marker を worktree の親に置く。wx のレイアウトではそこが必ず slot ディレクトリである。
// worktree の外に置くことで、中断した削除を再試行するときも所有権を証明できる。worktree が消えていても marker は残るためである。
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

// OwnershipMarkerName は、multi-repository workspace の bundle から slot の marker を除くために internal/daemon が使う公開名である。
// marker は bundle の root にあたる slot ディレクトリにあり、archive と復元前の prune のどちらでも残す必要がある。
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

// RegisteredWorktreeLockReason は target に対する Git の lock 理由を返す。
// found は、Git がその path に登録を持たないとき false になる。削除を途中まで進めた後がこれに当たる。
func RegisteredWorktreeLockReason(ctx context.Context, runner *gitx.Runner, mainPath, target string) (reason string, found bool, err error) {
	reason, _, found, err = RegisteredWorktreeLockStatus(ctx, runner, mainPath, target)
	return reason, found, err
}

// RegisteredWorktreeLockStatus は RegisteredWorktreeLockReason の完全版である。
// lock されていない worktree と、空の理由で lock された worktree を区別する。削除を引き継ぐときにこの差が効く。
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
			// symlink alias 経由で到達する Git 登録は所有権の一致とみなさない。
			// 先に解決すると path のすり替えが見えなくなる。
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

// ValidateRegisteredWorktreeAt は、descriptor で束縛した target が持つ inode に対して Git の登録を検証する。
// RegisteredWorktreeLockStatus と違い可変な target の path 名を正規化しないため、root のすり替えで別ディレクトリを wx のものに見せかけられない。
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

// RegisteredWorktreeLockStatusAt は、owner 経由で得た target の inode と Git の各登録を突き合わせる。
// root の外の登録は、symlink alias を辿って解決せずに無視する。
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
