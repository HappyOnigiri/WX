package archive

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

// workspaceSnapshotDirectory は worktree root 内に置く。pin 済み root descriptor、
// holdVerifiedRootForPath の削除前検査、下の決定的なパス再計算はすべてこの root を基準にする。
// "_" 接頭辞により orphan scan の対象外になる。scan は "_" で始まらない最上位項目を workspace とみなす。
const workspaceSnapshotDirectory = "_recovery/workspace-snapshots"

// ErrWorkspaceSnapshotIntegrity は archive 本文の破損・置換・読み取り障害を表す。
// 期限切れや metadata 不足とは区別する。呼び出し元はこれを見て、新しい worktree への自動 fresh 再開へ倒さない失敗として扱う。
var ErrWorkspaceSnapshotIntegrity = errors.New("workspace snapshot integrity check failed")

// ErrWorkspaceSnapshotExpired は保存期限切れ、または ARCHIVED でない snapshot を表す。
// 呼び出し元はこれを「復元の材料が無い」ことと解釈し、path・権限・DB の失敗とは区別する。
var ErrWorkspaceSnapshotExpired = errors.New("workspace snapshot is no longer available")

// SnapshotWorkspaceAt は daemon が multi-repository slot に使う descriptor 束縛版である。 owner は ownershipRoot に対応する manager 所有の root
// descriptor でなければならない。 bundle の読み取りと archive の書き込みを同じ物理 root に限定し、返す ArchivePath を SQLite に commit
// する前にパス名の置換を拒否する。
func SnapshotWorkspaceAt(ctx context.Context, bundleRoot, ownershipRoot, rootID string, owner *os.Root, sessionID string, excluded []string, expiry time.Time) (state.WorkspaceSnapshot, error) {
	if rootID == "" {
		return state.WorkspaceSnapshot{}, errors.New("workspace ownership root generation is required")
	}
	if owner == nil {
		return state.WorkspaceSnapshot{}, errors.New("workspace ownership root descriptor is nil")
	}
	if !domain.IsWithin(ownershipRoot, bundleRoot) {
		return state.WorkspaceSnapshot{}, errors.New("workspace bundle is outside wx ownership root")
	}
	exclusions, err := normalizeWorkspaceExclusions(excluded)
	if err != nil {
		return state.WorkspaceSnapshot{}, err
	}
	if err := verifyPinnedRootPath(ownershipRoot, owner); err != nil {
		return state.WorkspaceSnapshot{}, err
	}
	if err := owner.MkdirAll(filepath.FromSlash(workspaceSnapshotDirectory), 0o700); err != nil {
		return state.WorkspaceSnapshot{}, fmt.Errorf("create workspace recovery directory safely: %w", err)
	}
	root, rootErr := filepath.Abs(filepath.Clean(ownershipRoot))
	if rootErr != nil {
		return state.WorkspaceSnapshot{}, rootErr
	}
	bundlePath, bundleErr := filepath.Abs(filepath.Clean(bundleRoot))
	if bundleErr != nil {
		return state.WorkspaceSnapshot{}, bundleErr
	}
	relative, relErr := domain.RelativeWithin(root, bundlePath)
	if relErr != nil {
		return state.WorkspaceSnapshot{}, errors.New("workspace bundle is outside pinned wx ownership root")
	}
	bundle, err := domain.OpenRootAt(owner, relative)
	if err != nil {
		return state.WorkspaceSnapshot{}, fmt.Errorf("open pinned workspace bundle: %w", err)
	}
	defer func() { _ = bundle.Close() }()
	archiveRel := workspaceSnapshotRelativePath(sessionID)
	temporaryRel := archiveRel + ".tmp-" + domain.StableID(sessionID, state.FormatTime(time.Now()))
	output, err := owner.OpenFile(filepath.FromSlash(temporaryRel), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return state.WorkspaceSnapshot{}, err
	}
	keepTemporary := true
	defer func() {
		_ = output.Close()
		if keepTemporary {
			_ = owner.Remove(filepath.FromSlash(temporaryRel))
		}
	}()
	hasher := sha256.New()
	writer := tar.NewWriter(io.MultiWriter(output, hasher))
	err = writeWorkspaceArchiveEntries(ctx, bundle, writer, exclusions)
	if err == nil {
		err = writer.Close()
	} else {
		_ = writer.Close()
	}
	if err != nil {
		return state.WorkspaceSnapshot{}, err
	}
	if err := output.Sync(); err != nil {
		return state.WorkspaceSnapshot{}, err
	}
	if err := output.Close(); err != nil {
		return state.WorkspaceSnapshot{}, err
	}
	if err := verifyPinnedRootPath(ownershipRoot, owner); err != nil {
		return state.WorkspaceSnapshot{}, err
	}
	if err := owner.Rename(filepath.FromSlash(temporaryRel), filepath.FromSlash(archiveRel)); err != nil {
		return state.WorkspaceSnapshot{}, err
	}
	keepTemporary = false
	if directory, err := owner.Open(filepath.FromSlash(workspaceSnapshotDirectory)); err == nil {
		err = directory.Sync()
		_ = directory.Close()
		if err != nil {
			return state.WorkspaceSnapshot{}, err
		}
	} else {
		return state.WorkspaceSnapshot{}, err
	}
	if err := verifyPinnedRootPath(ownershipRoot, owner); err != nil {
		return state.WorkspaceSnapshot{}, err
	}
	created := time.Now().UTC()
	return state.WorkspaceSnapshot{
		SessionID: sessionID, RootID: rootID, RelPath: filepath.FromSlash(archiveRel),
		ArchivePath: filepath.Join(ownershipRoot, filepath.FromSlash(archiveRel)),
		SHA256:      hex.EncodeToString(hasher.Sum(nil)), Status: "ARCHIVED",
		CreatedAt: state.FormatTime(created), ExpiresAt: state.FormatTime(expiry),
	}, nil
}

func writeWorkspaceArchiveEntries(ctx context.Context, bundle *os.Root, writer *tar.Writer, exclusions []string) error {
	return fs.WalkDir(bundle.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		rel, err := archiveRelative(name)
		if err != nil {
			return err
		}
		if workspacePathExcluded(rel, exclusions) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		return writeWorkspaceArchiveEntry(bundle, writer, rel, entry)
	})
}

func writeWorkspaceArchiveEntry(bundle *os.Root, writer *tar.Writer, rel string, entry fs.DirEntry) error {
	info, err := entry.Info()
	if err != nil {
		return err
	}
	var linkTarget string
	switch {
	case info.IsDir(), info.Mode().IsRegular():
	case info.Mode()&os.ModeSymlink != 0:
		linkTarget, err = bundle.Readlink(filepath.FromSlash(rel))
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("workspace root path %s has unsupported mode %s", rel, info.Mode())
	}
	header, err := tar.FileInfoHeader(info, linkTarget)
	if err != nil {
		return err
	}
	header.Name = rel
	header.Uid, header.Gid, header.Uname, header.Gname = 0, 0, "", ""
	header.AccessTime, header.ChangeTime = time.Time{}, time.Time{}
	if err := writer.WriteHeader(header); err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	file, err := bundle.Open(filepath.FromSlash(rel))
	if err != nil {
		return err
	}
	openedInfo, statErr := file.Stat()
	if statErr != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return fmt.Errorf("workspace root file %s changed while snapshotting", rel)
	}
	_, copyErr := io.Copy(writer, file)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// verifyPinnedRootPath は descriptor 束縛 archive が成功しても、ArchivePath が置換後の
// namespace を指す状態で commit されることを防ぐ。pin 済み descriptor が内容を守り、
// この検査が daemon 再起動後にも永続パスのメタデータを利用可能にする。
func verifyPinnedRootPath(path string, owner *os.Root) error {
	if owner == nil {
		return fmt.Errorf("%w: workspace archive owner descriptor is nil", state.ErrOwnership)
	}
	current, err := workspace.OpenPhysicalRoot(path)
	if err != nil {
		return fmt.Errorf("%w: workspace archive owner path changed: %w", state.ErrOwnership, err)
	}
	defer func() { _ = current.Close() }()
	heldInfo, err := owner.Lstat(".")
	if err != nil {
		return fmt.Errorf("%w: inspect pinned workspace archive owner: %w", state.ErrOwnership, err)
	}
	currentInfo, err := current.Lstat(".")
	if err != nil {
		return fmt.Errorf("%w: inspect workspace archive owner path: %w", state.ErrOwnership, err)
	}
	if !os.SameFile(heldInfo, currentInfo) {
		return fmt.Errorf("%w: workspace archive owner path names a different directory", state.ErrOwnership)
	}
	return nil
}

// ValidateWorkspaceSnapshotMetadataAt は archive 本文を読まずに、保存 metadata と実体の種別だけを検証する。
// 再開の可否を返す軽量な問い合わせのためにあり、成功しても内容の完全性は保証しない。
// 完全性は復元 worker の OpenVerifiedWorkspaceSnapshotAt が 1 度だけ確かめる。
func ValidateWorkspaceSnapshotMetadataAt(ownershipRoot string, owner *os.Root, snapshot state.WorkspaceSnapshot, at time.Time) error {
	if owner == nil {
		return errors.New("workspace snapshot ownership root descriptor is nil")
	}
	if err := verifyPinnedRootPath(ownershipRoot, owner); err != nil {
		return err
	}
	if _, _, err := workspaceSnapshotMetadataAt(owner, snapshot, at); err != nil {
		return err
	}
	return verifyPinnedRootPath(ownershipRoot, owner)
}

// ValidateWorkspaceSnapshotAt は metadata に加えて archive 本文の SHA256 まで検証する。
// doctor・snapshot 登録直後の確認・削除前検査のように、その時点の最新状態を要求する経路が使う。
func ValidateWorkspaceSnapshotAt(ctx context.Context, ownershipRoot string, owner *os.Root, snapshot state.WorkspaceSnapshot, at time.Time) error {
	verified, err := OpenVerifiedWorkspaceSnapshotAt(ctx, ownershipRoot, owner, snapshot, at)
	if err != nil {
		return err
	}
	return verified.Close()
}

// VerifiedWorkspaceSnapshot は SHA256 を検証済みの archive descriptor を、先頭へ巻き戻した状態で保持する。
// 復元先を変更する前に取得し、そのまま展開へ渡すことで path からの再 open と再 hash を避ける。
// owner は呼び出し元が開いた root descriptor で、Close が解放するのは archive の descriptor だけである。
type VerifiedWorkspaceSnapshot struct {
	ownershipRoot string
	owner         *os.Root
	file          *os.File
	stamp         domain.FileStamp
}

// OpenVerifiedWorkspaceSnapshotAt は archive を pin して SHA256 を 1 度だけ検証する。
// ctx の取消は完全性の失敗と区別してそのまま返し、破損・置換・読み取り障害は ErrWorkspaceSnapshotIntegrity で包む。
func OpenVerifiedWorkspaceSnapshotAt(ctx context.Context, ownershipRoot string, owner *os.Root, snapshot state.WorkspaceSnapshot, at time.Time) (*VerifiedWorkspaceSnapshot, error) {
	if owner == nil {
		return nil, errors.New("workspace snapshot ownership root descriptor is nil")
	}
	if err := verifyPinnedRootPath(ownershipRoot, owner); err != nil {
		return nil, err
	}
	file, err := openWorkspaceSnapshotAt(owner, snapshot, at)
	if err != nil {
		return nil, err
	}
	stamp, err := verifyWorkspaceSnapshotDigest(ctx, file, snapshot)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := verifyPinnedRootPath(ownershipRoot, owner); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &VerifiedWorkspaceSnapshot{ownershipRoot: ownershipRoot, owner: owner, file: file, stamp: stamp}, nil
}

// Close は検証済み archive の descriptor を解放する。二重呼び出しは無害で、owner の寿命は呼び出し元が持つ。
func (v *VerifiedWorkspaceSnapshot) Close() error {
	if v == nil || v.file == nil {
		return nil
	}
	file := v.file
	v.file = nil
	return file.Close()
}

// verifyUnchanged は hash 時に記録した属性と現在の descriptor を比べ、検証後の in-place 変更を検出する。
func (v *VerifiedWorkspaceSnapshot) verifyUnchanged() error {
	if v == nil || v.file == nil {
		return errors.New("workspace snapshot descriptor is closed")
	}
	stamp, err := domain.FileStampOf(v.file)
	if err != nil {
		return err
	}
	if stamp != v.stamp {
		return fmt.Errorf("%w: archive changed after verification", ErrWorkspaceSnapshotIntegrity)
	}
	return nil
}

// RestoreWorkspaceAt は archive を検証してから multi-repository bundle を復元する。
// 検証と展開の間に別の準備を挟む復元 worker は、OpenVerifiedWorkspaceSnapshotAt と RestoreVerifiedWorkspace を使う。
func RestoreWorkspaceAt(ctx context.Context, bundleRoot, targetOwnershipRoot string, targetRootHandle *os.Root, archiveOwnershipRoot string, archiveRootHandle *os.Root, snapshot state.WorkspaceSnapshot, excluded []string) error {
	if targetRootHandle == nil || archiveRootHandle == nil {
		return errors.New("workspace restore root descriptor is nil")
	}
	verified, err := OpenVerifiedWorkspaceSnapshotAt(ctx, archiveOwnershipRoot, archiveRootHandle, snapshot, time.Now())
	if err != nil {
		return err
	}
	defer func() { _ = verified.Close() }()
	return RestoreVerifiedWorkspace(ctx, verified, bundleRoot, targetOwnershipRoot, targetRootHandle, excluded)
}

// RestoreVerifiedWorkspace は検証済み handle の内容だけを target へ展開する。
// prune を始める前と展開を終えた後に archive の同一性を確かめ、途中で入れ替わった archive の内容を残さない。
func RestoreVerifiedWorkspace(ctx context.Context, verified *VerifiedWorkspaceSnapshot, bundleRoot, targetOwnershipRoot string, targetRootHandle *os.Root, excluded []string) error {
	if verified == nil || verified.file == nil {
		return errors.New("workspace restore requires a verified snapshot")
	}
	if targetRootHandle == nil {
		return errors.New("workspace restore root descriptor is nil")
	}
	exclusions, err := normalizeWorkspaceExclusions(excluded)
	if err != nil {
		return err
	}
	if !domain.IsWithin(targetOwnershipRoot, bundleRoot) {
		return errors.New("workspace restore target is outside wx ownership root")
	}
	if err := verifyPinnedRootPath(targetOwnershipRoot, targetRootHandle); err != nil {
		return err
	}
	if err := verifyPinnedRootPath(verified.ownershipRoot, verified.owner); err != nil {
		return err
	}
	if err := verified.verifyUnchanged(); err != nil {
		return err
	}
	root, err := openWorkspaceRestoreTarget(bundleRoot, targetOwnershipRoot, targetRootHandle)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if _, err := verified.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := pruneWorkspaceRoot(root, ".", exclusions); err != nil {
		return err
	}
	if err := restoreWorkspaceEntries(ctx, root, verified.file, exclusions); err != nil {
		return err
	}
	if err := verified.verifyUnchanged(); err != nil {
		return err
	}
	if err := verifyPinnedRootPath(targetOwnershipRoot, targetRootHandle); err != nil {
		return err
	}
	return verifyPinnedRootPath(verified.ownershipRoot, verified.owner)
}

func openWorkspaceRestoreTarget(bundleRoot, targetOwnershipRoot string, targetRootHandle *os.Root) (*os.Root, error) {
	ownershipAbs, absErr := filepath.Abs(filepath.Clean(targetOwnershipRoot))
	if absErr != nil {
		return nil, absErr
	}
	bundleAbs, absErr := filepath.Abs(filepath.Clean(bundleRoot))
	if absErr != nil {
		return nil, absErr
	}
	relative, relErr := domain.RelativeWithin(ownershipAbs, bundleAbs)
	if relErr != nil {
		return nil, errors.New("workspace restore target is outside pinned wx ownership root")
	}
	root, err := domain.OpenRootAt(targetRootHandle, relative)
	if err != nil {
		return nil, fmt.Errorf("open pinned workspace restore target: %w", err)
	}
	return root, nil
}

func restoreWorkspaceEntries(ctx context.Context, root *os.Root, archiveFile *os.File, exclusions []string) error {
	reader := tar.NewReader(archiveFile)
	seen := map[string]byte{}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if err := restoreWorkspaceEntry(root, reader, header, exclusions, seen); err != nil {
			return err
		}
	}
	return nil
}

func restoreWorkspaceEntry(root *os.Root, reader *tar.Reader, header *tar.Header, exclusions []string, seen map[string]byte) error {
	rel, err := archiveRelative(header.Name)
	if err != nil {
		return err
	}
	if workspacePathExcluded(rel, exclusions) {
		return fmt.Errorf("workspace archive path %s overlaps an excluded repository or shared link", rel)
	}
	if _, duplicate := seen[rel]; duplicate {
		return fmt.Errorf("duplicate workspace archive path %s", rel)
	}
	for parent := path.Dir(rel); parent != "."; parent = path.Dir(parent) {
		if seen[parent] == tar.TypeSymlink {
			return fmt.Errorf("workspace archive path %s descends through symlink %s", rel, parent)
		}
	}
	seen[rel] = header.Typeflag
	osRel := filepath.FromSlash(rel)
	parent := filepath.Dir(osRel)
	if parent != "." {
		if err := root.MkdirAll(parent, 0o700); err != nil {
			return err
		}
	}
	switch header.Typeflag {
	case tar.TypeDir:
		if info, err := root.Lstat(osRel); errors.Is(err, os.ErrNotExist) {
			if err := root.Mkdir(osRel, 0o700); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("workspace archive directory collision %s", rel)
		}
	case tar.TypeReg, tar.TypeRegA:
		return restoreWorkspaceRegularFile(root, reader, osRel, rel, header)
	case tar.TypeSymlink:
		if err := root.Symlink(header.Linkname, osRel); err != nil {
			return err
		}
	default:
		return fmt.Errorf("workspace archive path %s has unsupported tar type %d", rel, header.Typeflag)
	}
	return nil
}

func restoreWorkspaceRegularFile(root *os.Root, reader *tar.Reader, osRel, rel string, header *tar.Header) error {
	if header.Size < 0 {
		return fmt.Errorf("workspace archive file %s has invalid size", rel)
	}
	file, err := root.OpenFile(osRel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	written, copyErr := io.CopyN(file, reader, header.Size)
	if copyErr == nil && written != header.Size {
		copyErr = io.ErrUnexpectedEOF
	}
	if copyErr == nil {
		copyErr = file.Chmod(os.FileMode(header.Mode) & os.ModePerm)
	}
	if copyErr == nil {
		copyErr = file.Sync()
	}
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// DeleteWorkspaceSnapshotAt は checksum と決定的なパスを検証した後、pin 済み root descriptor 経由で
// recovery archive を削除する。
func DeleteWorkspaceSnapshotAt(ctx context.Context, ownershipRoot string, owner *os.Root, snapshot state.WorkspaceSnapshot) error {
	if owner == nil {
		return errors.New("workspace snapshot ownership root descriptor is nil")
	}
	if err := verifyPinnedRootPath(ownershipRoot, owner); err != nil {
		return err
	}
	rel := filepath.FromSlash(workspaceSnapshotRelativePath(snapshot.SessionID))
	if filepath.Clean(snapshot.RelPath) != filepath.Clean(rel) {
		return errors.New("workspace snapshot path does not match its session")
	}
	if _, err := owner.Lstat(rel); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := ValidateWorkspaceSnapshotAt(ctx, ownershipRoot, owner, snapshot, time.Time{}); err != nil {
		return err
	}
	if err := owner.Remove(rel); err != nil {
		return err
	}
	if err := verifyPinnedRootPath(ownershipRoot, owner); err != nil {
		return err
	}
	if directory, err := owner.Open(filepath.FromSlash(workspaceSnapshotDirectory)); err == nil {
		syncErr := directory.Sync()
		_ = directory.Close()
		return syncErr
	}
	return nil
}

// workspaceSnapshotMetadataAt は archive 本文を読まずに、状態・期限・決定的な path・実体の種別を確かめる。
// 返す rel と FileInfo は、この検査を通った実体を再解決せずに open するために使う。
func workspaceSnapshotMetadataAt(owner *os.Root, snapshot state.WorkspaceSnapshot, at time.Time) (string, os.FileInfo, error) {
	if snapshot.Status != "ARCHIVED" {
		return "", nil, fmt.Errorf("%w: status is %s", ErrWorkspaceSnapshotExpired, snapshot.Status)
	}
	if !at.IsZero() {
		expiry, err := time.Parse(time.RFC3339Nano, snapshot.ExpiresAt)
		if err != nil || !expiry.After(at) {
			return "", nil, fmt.Errorf("%w: retention has elapsed", ErrWorkspaceSnapshotExpired)
		}
	}
	rel := filepath.FromSlash(workspaceSnapshotRelativePath(snapshot.SessionID))
	if filepath.Clean(snapshot.RelPath) != filepath.Clean(rel) {
		return "", nil, errors.New("workspace snapshot path does not match its session")
	}
	if owner == nil {
		return "", nil, errors.New("workspace snapshot ownership root descriptor is nil")
	}
	info, err := owner.Lstat(rel)
	if err != nil {
		return "", nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", nil, errors.New("workspace snapshot artifact is not a regular file")
	}
	return rel, info, nil
}

func openWorkspaceSnapshotAt(owner *os.Root, snapshot state.WorkspaceSnapshot, at time.Time) (*os.File, error) {
	rel, info, err := workspaceSnapshotMetadataAt(owner, snapshot, at)
	if err != nil {
		return nil, err
	}
	file, err := owner.Open(rel)
	if err != nil {
		return nil, err
	}
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return nil, fmt.Errorf("%w: artifact changed while opening", ErrWorkspaceSnapshotIntegrity)
	}
	return file, nil
}

// workspaceSnapshotHashChunk は ctx の取消を確認する読み取り単位である。
const workspaceSnapshotHashChunk = 1 << 20

// verifyWorkspaceSnapshotDigest は descriptor 全体の SHA256 を metadata と突き合わせ、offset を先頭へ戻す。
// 展開の前後で比較する stamp を返す。取消は完全性の失敗と区別してそのまま返す。
func verifyWorkspaceSnapshotDigest(ctx context.Context, file *os.File, snapshot state.WorkspaceSnapshot) (domain.FileStamp, error) {
	hasher := sha256.New()
	buffer := make([]byte, workspaceSnapshotHashChunk)
	for {
		if err := ctx.Err(); err != nil {
			return domain.FileStamp{}, err
		}
		read, err := file.Read(buffer)
		if read > 0 {
			_, _ = hasher.Write(buffer[:read])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return domain.FileStamp{}, ctxErr
			}
			return domain.FileStamp{}, fmt.Errorf("%w: read archive: %w", ErrWorkspaceSnapshotIntegrity, err)
		}
	}
	if hex.EncodeToString(hasher.Sum(nil)) != snapshot.SHA256 {
		return domain.FileStamp{}, fmt.Errorf("%w: checksum does not match metadata", ErrWorkspaceSnapshotIntegrity)
	}
	stamp, err := domain.FileStampOf(file)
	if err != nil {
		return domain.FileStamp{}, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return domain.FileStamp{}, err
	}
	return stamp, nil
}

func workspaceSnapshotRelativePath(sessionID string) string {
	name := domain.StableID("workspace-snapshot", sessionID) + ".tar"
	return path.Join(workspaceSnapshotDirectory, name)
}

func normalizeWorkspaceExclusions(values []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		rel, err := archiveRelative(filepath.ToSlash(value))
		if err != nil {
			return nil, fmt.Errorf("unsafe workspace snapshot exclusion %q: %w", value, err)
		}
		if !seen[rel] {
			seen[rel] = true
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	return out, nil
}

func archiveRelative(value string) (string, error) {
	if value == "" || strings.ContainsRune(value, '\x00') || strings.Contains(value, "\\") {
		return "", fmt.Errorf("unsafe archive path %q", value)
	}
	clean := path.Clean(value)
	if clean == "." || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") || clean != value {
		return "", fmt.Errorf("unsafe archive path %q", value)
	}
	return clean, nil
}

func workspacePathExcluded(rel string, exclusions []string) bool {
	for _, excluded := range exclusions {
		if rel == excluded || strings.HasPrefix(rel, excluded+"/") {
			return true
		}
	}
	return false
}

func workspacePathContainsExclusion(rel string, exclusions []string) bool {
	for _, excluded := range exclusions {
		if strings.HasPrefix(excluded, rel+"/") {
			return true
		}
	}
	return false
}

func pruneWorkspaceRoot(root *os.Root, directory string, exclusions []string) error {
	entries, err := fs.ReadDir(root.FS(), directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		rel := entry.Name()
		if directory != "." {
			rel = path.Join(directory, entry.Name())
		}
		if workspacePathExcluded(rel, exclusions) {
			continue
		}
		if workspacePathContainsExclusion(rel, exclusions) {
			if !entry.IsDir() {
				return fmt.Errorf("workspace exclusion ancestor %s is not a directory", rel)
			}
			if err := pruneWorkspaceRoot(root, rel, exclusions); err != nil {
				return err
			}
			continue
		}
		if err := root.RemoveAll(filepath.FromSlash(rel)); err != nil {
			return err
		}
	}
	return nil
}
