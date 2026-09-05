package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SlotID と SessionID は未使用のため削除した。package 外から参照されず、internal/state.Store は全境界で plain string の slot/session ID を使う。
// discovery.Workspace/Repository と呼び出し元が使う WorkspaceID と RepositoryID は残す。
type (
	WorkspaceID  string
	RepositoryID string
)

func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// shortIDAlphabet は lowercase base36 である。ID は directory 名で APFS は既定で case-insensitive のため、大文字を含めると SQLite では別 ID が disk 上で衝突する。
const shortIDAlphabet = "0123456789abcdefghijklmnopqrstuvwxyz"

// ShortIDLength は NewShortID 出力の固定幅である。6桁 base36 は約21.8億通りで、single-user install の live slot/session row 数では衝突確率を1%未満に保つ。
const ShortIDLength = 6

// NewShortID は crypto/rand から得た ShortIDLength 文字の lowercase base36 identifier を返す。
// alphabet size の最大倍数以上の byte を modulo 縮約せず拒否し、各文字を一様分布にする。
func NewShortID() (string, error) {
	const limit = 256 - (256 % len(shortIDAlphabet))
	out := make([]byte, 0, ShortIDLength)
	var buffer [ShortIDLength]byte
	for len(out) < ShortIDLength {
		if _, err := rand.Read(buffer[:]); err != nil {
			return "", fmt.Errorf("generate short id: %w", err)
		}
		for _, b := range buffer {
			if int(b) >= limit {
				continue
			}
			out = append(out, shortIDAlphabet[int(b)%len(shortIDAlphabet)])
			if len(out) == ShortIDLength {
				break
			}
		}
	}
	return string(out), nil
}

// ValidShortID は value が NewShortID と同じ形式かを返す。orphan scan はこれで worktree root 配下の
// wx namespace を判定し、short ID 以外を wx 作成 slot location と扱わない。
// 生成値の path component 検査はここではなく呼び出し元（daemon.validateLayoutComponent）が行う。
func ValidShortID(value string) bool {
	if len(value) != ShortIDLength {
		return false
	}
	for i := 0; i < len(value); i++ {
		if !strings.ContainsRune(shortIDAlphabet, rune(value[i])) {
			return false
		}
	}
	return true
}

func StableID(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

type CanonicalPath string

func Canonicalize(path string) (CanonicalPath, error) {
	if path == "" {
		return "", errors.New("path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("absolute path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("canonicalize %q: %w", path, err)
	}
	return CanonicalPath(filepath.Clean(resolved)), nil
}

func IsWithin(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && rel != "." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// RelativeWithin は root に対する path の位置を求め、root 自身または外部を指す結果を拒否する。absolute path、`.`, `..`, `../` で始まる値が対象である。
// Go 1.26 の filepath.IsLocal は `.` 以外を拒否するため `.` だけを明示検査する。path == root を許す呼び出し元はこれを使わない。
func RelativeWithin(root, path string) (string, error) {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return "", err
	}
	if relative == "." || !filepath.IsLocal(relative) {
		return "", fmt.Errorf("%s is outside %s", path, root)
	}
	return relative, nil
}

// ValidWxLockReason は reason が wx 自身が slotID に書き込む `git worktree lock` の
// 理由（READY、PREPARING、RESTORING）のいずれかであるかを返す。
func ValidWxLockReason(reason, slotID string) bool {
	return reason == "wx:"+slotID+":READY" || reason == "wx:"+slotID+":PREPARING" || reason == "wx:"+slotID+":RESTORING"
}

// OpenOwnedRoot は後続 filesystem 操作を検証済み ownership root に束縛し、その descriptor 相対の owned path を返す。
// filesystem root から開くことで configured root の symlink を拒否する。
// component を独自 Root として開き直し、この関数の返却後も rename/置換をまたいで directory を pin する。
func OpenOwnedRoot(root, path string) (*os.Root, string, error) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, "", fmt.Errorf("resolve ownership root: %w", err)
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, "", fmt.Errorf("resolve owned path: %w", err)
	}
	absoluteRoot, absolutePath = filepath.Clean(absoluteRoot), filepath.Clean(absolutePath)
	if absoluteRoot != absolutePath && !IsWithin(absoluteRoot, absolutePath) {
		return nil, "", errors.New("path is outside wx ownership root")
	}
	filesystemRoot, rootRelative, err := openFilesystemRoot(absoluteRoot)
	if err != nil {
		return nil, "", err
	}
	info, err := PhysicalPathInfo(filesystemRoot, rootRelative)
	if err != nil {
		_ = filesystemRoot.Close()
		return nil, "", fmt.Errorf("validate wx ownership root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		_ = filesystemRoot.Close()
		return nil, "", errors.New("wx ownership root is not a physical directory")
	}
	ownedRoot, err := filesystemRoot.OpenRoot(rootRelative)
	closeErr := filesystemRoot.Close()
	if err != nil {
		return nil, "", fmt.Errorf("open wx ownership root: %w", err)
	}
	if closeErr != nil {
		_ = ownedRoot.Close()
		return nil, "", closeErr
	}
	openedInfo, err := ownedRoot.Lstat(".")
	if err != nil {
		_ = ownedRoot.Close()
		return nil, "", fmt.Errorf("revalidate wx ownership root: %w", err)
	}
	if openedInfo.Mode()&os.ModeSymlink != 0 || !openedInfo.IsDir() || !os.SameFile(info, openedInfo) {
		_ = ownedRoot.Close()
		return nil, "", errors.New("wx ownership root changed while opening")
	}
	relative, err := filepath.Rel(absoluteRoot, absolutePath)
	if err != nil {
		_ = ownedRoot.Close()
		return nil, "", err
	}
	if relative == "" {
		relative = "."
	}
	return ownedRoot, relative, nil
}

func openFilesystemRoot(absolute string) (*os.Root, string, error) {
	volumeRoot := filepath.VolumeName(absolute) + string(filepath.Separator)
	handle, err := os.OpenRoot(volumeRoot)
	if err != nil {
		return nil, "", err
	}
	relative := strings.TrimPrefix(filepath.Clean(absolute), volumeRoot)
	if relative == "" {
		relative = "."
	}
	return handle, relative, nil
}

// PhysicalPathInfo は、既に開いた Root からの相対 path の全成分で symlink を拒否し、
// 最終成分の metadata を返す。同じ filesystem-root descriptor で検査し、別途評価した
// 字句 path を物理的な包含の証明として扱わない。
func PhysicalPathInfo(root *os.Root, relative string) (os.FileInfo, error) {
	current := "."
	clean := filepath.Clean(relative)
	for _, component := range strings.Split(clean, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := root.Lstat(current)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("symlink component in physical path %s", current)
		}
		if !info.IsDir() && current != clean {
			return nil, fmt.Errorf("non-directory component in physical path %s", current)
		}
	}
	return root.Lstat(clean)
}

// ValidatePhysicalPath は存在する path 成分の全てで symbolic link を拒否する。
// 安全に作成できるよう、allowMissingLeaf が真なら最後の成分の欠落を許す。
func ValidatePhysicalPath(path string, allowMissingLeaf bool) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(absolute)
	current := volume + string(filepath.Separator)
	components := strings.Split(strings.TrimPrefix(absolute, current), string(filepath.Separator))
	for index, component := range components {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if allowMissingLeaf && errors.Is(err, os.ErrNotExist) && index == len(components)-1 {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink component in physical path %s", current)
		}
	}
	return nil
}

// EnsurePhysicalDirectory は filesystem-root descriptor 配下に成分ごとに directory を作り、
// 各成分で symlink を拒否する。
func EnsurePhysicalDirectory(path string, perm os.FileMode) error {
	root, err := EnsurePhysicalDirectoryRoot(path, perm)
	if err != nil {
		return err
	}
	return root.Close()
}

// EnsurePhysicalDirectoryRoot は EnsurePhysicalDirectory の descriptor 保持版である。filesystem-root
// descriptor を解放する前に最終 directory を開いて最後の Lstat と比較し、作成後の rename/置換で
// 呼び出し元の最初の write が無関係な pathname に向かないようにする。対応対象は設計上 darwin と
// linux に限り（physical_unix.go 参照）、未検証の他 platform 向け fallback は実装しない。
// commentlint:allow-long -- 契約と安全条件を保持する説明のため
func EnsurePhysicalDirectoryRoot(path string, perm os.FileMode) (*os.Root, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return ensurePhysicalDirectoryRootPlatform(filepath.Clean(absolute), perm)
}

// 設計文書の state machine と package 構成に対する変更として、ここにあった SlotState / SessionState
// と CanTransitionSlot は削除した。状態の権威は internal/state.Store だけであり、全遷移を SQLite の
// compare-and-swap（state IN (...) と RowsAffected()==1）で検証する。旧 enum が表せなかった STALE、
// REMOVING、RETIRING、COLD、slot_repositories の PREPARE_RUNNING/RESTORE_RUNNING も含む。
//
// commentlint:allow-long -- 状態遷移の権威と削除理由を保守時に確認できるようにするため
