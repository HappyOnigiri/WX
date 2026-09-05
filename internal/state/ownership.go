package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/HappyOnigiri/WX/internal/domain"
)

// ErrOwnership は所有権証明の失敗を示す。呼び出し元は filesystem artifact を残し、owner を推測せず reconciliation で quarantine するフェイルクローズ結果として扱う。
var ErrOwnership = errors.New("wx ownership validation failed")

// OwnershipValidator は workspace/archive 操作に必要な小さな state contract である。Store は read-only transaction で実装する。
// transaction は意図的に Store.writer を取らない。この呼出し中に daemon は Git common-directory lock を持つ一方、state writer はそれを取らないため lock order を逆転させない。
type OwnershipValidator interface {
	ValidateWorktreeOwnership(context.Context, WorktreeOwnershipRequest) (WorktreeOwnership, error)
}

// WorktreeOwnershipRequest は SQLite と同じく root generation、slot の root 相対 path、repository directory 名、呼び出し元が保持する inode identity で worktree を位置付ける。
// この境界を absolute pathname は越えないため、pathname の置換では別 directory を同一と比較できない。
type WorktreeOwnershipRequest struct {
	SlotID       string
	WorkspaceID  string
	RepositoryID string
	RootID       string
	SlotRelPath  string
	DirName      string
	// DirIdentity は呼び出し元が開いた directory の dev:ino identity である。指定時は記録値が存在し一致しなければならず、空 record は失敗とする。
	// 空にできるのは worktree 作成前の prepare 検査だけで、その時だけは identity を提示できない。
	DirIdentity             string
	CommonDir               string
	AllowedSlotStates       []string
	AllowedRepositoryStates []string
}

// WorktreeOwnership は ValidateWorktreeOwnership が証明した durable identity である。組み立てた absolute path ではなく記録済み location を返し、diagnostics が応答した root generation を示せる。
type WorktreeOwnership struct {
	SlotID           string
	WorkspaceID      string
	RepositoryID     string
	Generation       int
	RootID           string
	RootPath         string
	SlotRelPath      string
	DirName          string
	WorkspaceRoot    string
	MainWorktreePath string
	CommonDir        string
	RelativePath     string
	SlotState        string
	RepositoryState  string
	// DirIdentity は SQLite が worktree directory に記録した identity で、prepare/restore 完了までは空である。中断した prepare の再実行のように identity を要求できない呼び出し元も、
	// 返却値を使って記録 identity の異なる directory を拒否できる。
	DirIdentity string
}

// SlotOwnershipRequest は ValidateWorktreeOwnership で証明できる repository worktree を持たない操作のために slot directory を位置付ける。
type SlotOwnershipRequest struct {
	SlotID            string
	WorkspaceID       string
	RootID            string
	RelPath           string
	DirIdentity       string
	AllowedSlotStates []string
}

var _ OwnershipValidator = (*Store)(nil)

// validateOwnershipRelative は root 相対 location にならない値を拒否する。空、absolute path、unclean な綴り、root/slot directory 自身を指す `.`, `..` による escape が対象である。
func validateOwnershipRelative(kind, value string) error {
	if value == "" {
		return fmt.Errorf("%s is empty", kind)
	}
	if value == "." || filepath.Clean(value) != value || !filepath.IsLocal(value) {
		return fmt.Errorf("%s %q is not a safe root-relative location", kind, value)
	}
	return nil
}

// validateOwnershipComponent はさらに一つだけの path component を要求する。repository directory 名は slot の直接の子でなければならない。
// filepath.IsLocal は `a/b` を受け入れるため、これがないと記録済み dir_name が任意の深さに worktree を置けて UNIQUE(slot_id, dir_name) でも検出できない。
func validateOwnershipComponent(kind, value string) error {
	if err := validateOwnershipRelative(kind, value); err != nil {
		return err
	}
	if strings.ContainsRune(value, filepath.Separator) {
		return fmt.Errorf("%s %q is not a single path component", kind, value)
	}
	return nil
}

// ValidateWorktreeOwnership は、呼び出し元の worktree が指定 slot/repository pair の記録済み directory と完全に同一であることを証明する。
// root generation、root 相対 slot path、directory 名、inode、workspace 所属、common Git directory、両 durable state machine の許可状態を検査する。
func (s *Store) ValidateWorktreeOwnership(ctx context.Context, req WorktreeOwnershipRequest) (WorktreeOwnership, error) {
	if s == nil || s.db == nil {
		return WorktreeOwnership{}, ownershipFailure("state store is unavailable")
	}
	if req.SlotID == "" || req.RepositoryID == "" || req.RootID == "" || req.CommonDir == "" {
		return WorktreeOwnership{}, ownershipFailure("slot, repository, root, and common directory are required")
	}
	if err := validateOwnershipRelative("requested slot path", req.SlotRelPath); err != nil {
		return WorktreeOwnership{}, ownershipFailure(err.Error())
	}
	if err := validateOwnershipComponent("requested repository directory", req.DirName); err != nil {
		return WorktreeOwnership{}, ownershipFailure(err.Error())
	}
	if len(req.AllowedSlotStates) == 0 || len(req.AllowedRepositoryStates) == 0 {
		return WorktreeOwnership{}, ownershipFailure("eligible slot and repository states are required")
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return WorktreeOwnership{}, ownershipDatabaseFailure(err)
	}
	defer func() { _ = tx.Rollback() }()

	var out WorktreeOwnership
	var workspaceRoot, mainWorktreePath, commonDir string
	var workspaceID, slotState, repositoryID, repositoryState, relativePath string
	var storedDirIdentity string
	err = tx.QueryRowContext(ctx, `
		SELECT sl.id,sl.workspace_id,sl.generation,sl.root_id,rt.path,sl.rel_path,sl.state,
		       sr.repository_id,sr.dir_name,COALESCE(sr.dir_identity,''),sr.state,
		       w.root_path,r.main_worktree_path,r.common_git_dir,wr.relative_path
		FROM slots sl
		JOIN roots rt ON rt.id=sl.root_id
		JOIN slot_repositories sr ON sr.slot_id=sl.id
		JOIN workspaces w ON w.id=sl.workspace_id
		JOIN repositories r ON r.id=sr.repository_id
		JOIN workspace_repositories wr ON wr.workspace_id=sl.workspace_id
		                          AND wr.repository_id=sr.repository_id
		WHERE sl.id=? AND sr.repository_id=?`, req.SlotID, req.RepositoryID).
		Scan(&out.SlotID, &workspaceID, &out.Generation, &out.RootID, &out.RootPath, &out.SlotRelPath, &slotState,
			&repositoryID, &out.DirName, &storedDirIdentity, &repositoryState, &workspaceRoot,
			&mainWorktreePath, &commonDir, &relativePath)
	if err != nil {
		return WorktreeOwnership{}, ownershipDatabaseFailure(err)
	}
	if err := tx.Commit(); err != nil {
		return WorktreeOwnership{}, ownershipDatabaseFailure(err)
	}

	if workspaceID == "" || repositoryID != req.RepositoryID {
		return WorktreeOwnership{}, ownershipFailure("slot/repository workspace association is incomplete")
	}
	if req.WorkspaceID != "" && req.WorkspaceID != workspaceID {
		return WorktreeOwnership{}, ownershipFailure("slot belongs to a different workspace")
	}
	if !slices.Contains(req.AllowedSlotStates, slotState) {
		return WorktreeOwnership{}, ownershipFailure(fmt.Sprintf("slot %s is in ineligible state %s", req.SlotID, slotState))
	}
	if !slices.Contains(req.AllowedRepositoryStates, repositoryState) {
		return WorktreeOwnership{}, ownershipFailure(fmt.Sprintf("repository %s is in ineligible state %s", req.RepositoryID, repositoryState))
	}
	if out.RootID != req.RootID {
		return WorktreeOwnership{}, ownershipFailure("requested worktree root generation does not match the SQLite slot")
	}
	if err := validateOwnershipRelative("recorded slot path", out.SlotRelPath); err != nil {
		return WorktreeOwnership{}, ownershipFailure(err.Error())
	}
	if err := validateOwnershipComponent("recorded repository directory", out.DirName); err != nil {
		return WorktreeOwnership{}, ownershipFailure(err.Error())
	}
	if out.SlotRelPath != req.SlotRelPath {
		return WorktreeOwnership{}, ownershipFailure("requested slot location does not match the SQLite slot")
	}
	if out.DirName != req.DirName {
		return WorktreeOwnership{}, ownershipFailure("requested repository directory does not match the SQLite slot")
	}
	// identity はフェイルクローズする。descriptor を持つ呼び出し元は記録 identity を見つけ、同じ inode でなければならない。record 欠落を一致扱いすると、この検査が置換した pathname-only proof を復活させる。
	if req.DirIdentity != "" {
		if storedDirIdentity == "" {
			return WorktreeOwnership{}, ownershipFailure("SQLite has no recorded worktree directory identity")
		}
		if storedDirIdentity != req.DirIdentity {
			return WorktreeOwnership{}, ownershipFailure("worktree directory identity does not match the SQLite record")
		}
	}

	canonicalWorkspaceRoot, err := canonicalExistingDirectory(workspaceRoot)
	if err != nil {
		return WorktreeOwnership{}, ownershipFailure(fmt.Sprintf("recorded workspace root: %v", err))
	}
	canonicalMain, err := canonicalExistingDirectory(mainWorktreePath)
	if err != nil {
		return WorktreeOwnership{}, ownershipFailure(fmt.Sprintf("recorded repository main path: %v", err))
	}
	canonicalCommon, err := canonicalExistingDirectory(commonDir)
	if err != nil {
		return WorktreeOwnership{}, ownershipFailure(fmt.Sprintf("recorded repository common directory: %v", err))
	}
	requestedCommon, err := canonicalExistingDirectory(req.CommonDir)
	if err != nil {
		return WorktreeOwnership{}, ownershipFailure(fmt.Sprintf("requested repository common directory: %v", err))
	}
	if canonicalCommon != requestedCommon {
		return WorktreeOwnership{}, ownershipFailure("requested common directory does not match the SQLite repository")
	}
	if err := validateWorkspaceRepositoryAssociation(canonicalWorkspaceRoot, canonicalMain, relativePath); err != nil {
		return WorktreeOwnership{}, ownershipFailure(err.Error())
	}

	out.WorkspaceID = workspaceID
	out.RepositoryID = repositoryID
	out.WorkspaceRoot = canonicalWorkspaceRoot
	out.MainWorktreePath = canonicalMain
	out.CommonDir = canonicalCommon
	out.RelativePath = filepath.Clean(relativePath)
	out.SlotState = slotState
	out.RepositoryState = repositoryState
	out.DirIdentity = storedDirIdentity
	return out, nil
}

// ValidateSlotOwnership は slot directory の削除または再作成前に使う。repository worktree がなく ValidateWorktreeOwnership で証明できない slot も、同じフェイルクローズの location/state 検査で扱う。
func (s *Store) ValidateSlotOwnership(ctx context.Context, req SlotOwnershipRequest) error {
	if s == nil || s.db == nil {
		return ownershipFailure("state store is unavailable")
	}
	if req.SlotID == "" || req.RootID == "" || len(req.AllowedSlotStates) == 0 {
		return ownershipFailure("slot, root, and eligible states are required")
	}
	if err := validateOwnershipRelative("requested slot path", req.RelPath); err != nil {
		return ownershipFailure(err.Error())
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ownershipDatabaseFailure(err)
	}
	defer func() { _ = tx.Rollback() }()
	var workspaceID, slotState, rootID, relPath, storedDirIdentity, rawRoot string
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(sl.workspace_id,''),sl.root_id,sl.rel_path,COALESCE(sl.dir_identity,''),sl.state,w.root_path
		FROM slots sl LEFT JOIN workspaces w ON w.id=sl.workspace_id
		WHERE sl.id=?`, req.SlotID).
		Scan(&workspaceID, &rootID, &relPath, &storedDirIdentity, &slotState, &rawRoot)
	if err != nil {
		return ownershipDatabaseFailure(err)
	}
	if err := tx.Commit(); err != nil {
		return ownershipDatabaseFailure(err)
	}
	if workspaceID == "" || rawRoot == "" || (req.WorkspaceID != "" && req.WorkspaceID != workspaceID) {
		return ownershipFailure("slot workspace association is incomplete or changed")
	}
	if !slices.Contains(req.AllowedSlotStates, slotState) {
		return ownershipFailure(fmt.Sprintf("slot %s is in ineligible state %s", req.SlotID, slotState))
	}
	if rootID != req.RootID {
		return ownershipFailure("requested worktree root generation does not match the SQLite slot")
	}
	if err := validateOwnershipRelative("recorded slot path", relPath); err != nil {
		return ownershipFailure(err.Error())
	}
	if relPath != req.RelPath {
		return ownershipFailure("requested slot location does not match the SQLite slot")
	}
	if req.DirIdentity != "" {
		if storedDirIdentity == "" {
			return ownershipFailure("SQLite has no recorded slot directory identity")
		}
		if storedDirIdentity != req.DirIdentity {
			return ownershipFailure("slot directory identity does not match the SQLite record")
		}
	}
	if _, err := canonicalExistingDirectory(rawRoot); err != nil {
		return ownershipFailure(fmt.Sprintf("recorded workspace root: %v", err))
	}
	return nil
}

func validateWorkspaceRepositoryAssociation(workspaceRoot, mainPath, relative string) error {
	if relative == "" || filepath.IsAbs(relative) {
		return errors.New("workspace/repository relative path is unsafe")
	}
	clean := filepath.Clean(relative)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return errors.New("workspace/repository relative path escapes workspace root")
	}
	expected, err := canonicalOwnershipPath(filepath.Join(workspaceRoot, clean))
	if err != nil {
		return fmt.Errorf("canonicalize workspace repository association: %w", err)
	}
	if expected != mainPath {
		return errors.New("workspace/repository relative path does not match repository main path")
	}
	return nil
}

// canonicalOwnershipPath は既存 component を全て解決し、欠落 suffix を lexical に追加する。prepare と中断した removal は欠落 worktree leaf を正当に検証するが、既存 symlink component は一切受け入れない。
func canonicalOwnershipPath(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("path is empty")
	}
	if !filepath.IsAbs(raw) {
		return "", errors.New("path is not absolute")
	}
	absolute, err := filepath.Abs(raw)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	missing := []string{}
	current := absolute
	for {
		_, statErr := os.Lstat(current)
		if statErr == nil {
			if err := domain.ValidatePhysicalPath(current, false); err != nil {
				return "", err
			}
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			resolved = filepath.Clean(resolved)
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return "", statErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", statErr
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func canonicalExistingDirectory(raw string) (string, error) {
	if raw == "" || !filepath.IsAbs(raw) {
		return "", errors.New("path is not absolute")
	}
	absolute := filepath.Clean(raw)
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("path is not an existing physical directory")
	}
	return canonicalOwnershipPath(absolute)
}

func ownershipFailure(message string) error {
	return fmt.Errorf("%w: %s", ErrOwnership, message)
}

func ownershipDatabaseFailure(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ownershipFailure("SQLite ownership row is missing")
	}
	return fmt.Errorf("%w: SQLite ownership query: %w", ErrOwnership, err)
}
