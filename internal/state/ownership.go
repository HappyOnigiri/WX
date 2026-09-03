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

// ErrOwnership identifies an ownership proof failure. Callers must treat this
// as a fail-closed result: the filesystem artifact is left in place so that
// reconciliation can quarantine it rather than guessing which owner it has.
var ErrOwnership = errors.New("wx ownership validation failed")

// OwnershipValidator is the small state contract required by workspace and
// archive operations. Store implements it using a read-only transaction. The
// transaction deliberately does not acquire Store.writer: daemon code holds
// the Git common-directory lock while calling this method, whereas state
// writers never acquire that lock, so the lock order cannot be reversed.
type OwnershipValidator interface {
	ValidateWorktreeOwnership(context.Context, WorktreeOwnershipRequest) (WorktreeOwnership, error)
}

type WorktreeOwnershipRequest struct {
	SlotID                  string
	WorkspaceID             string
	RepositoryID            string
	SlotPath                string
	WorktreePath            string
	CommonDir               string
	AllowedSlotStates       []string
	AllowedRepositoryStates []string
}

// WorktreeOwnership is the durable identity proved by a successful
// ValidateWorktreeOwnership call. The returned canonical paths are useful to
// diagnostics and make it explicit which physical paths were compared.
type WorktreeOwnership struct {
	SlotID           string
	WorkspaceID      string
	RepositoryID     string
	Generation       int
	SlotPath         string
	WorktreePath     string
	WorkspaceRoot    string
	MainWorktreePath string
	CommonDir        string
	RelativePath     string
	SlotState        string
	RepositoryState  string
}

type SlotOwnershipRequest struct {
	SlotID            string
	WorkspaceID       string
	Path              string
	AllowedSlotStates []string
}

var _ OwnershipValidator = (*Store)(nil)

// ValidateWorktreeOwnership proves that the caller's physical worktree is the
// exact path recorded for the requested slot/repository pair. It also proves
// that the repository belongs to the slot's workspace, that its common Git
// directory is the one recorded for that repository, and that both durable
// state machines are in caller-approved states.
func (s *Store) ValidateWorktreeOwnership(ctx context.Context, req WorktreeOwnershipRequest) (WorktreeOwnership, error) {
	if s == nil || s.db == nil {
		return WorktreeOwnership{}, ownershipFailure("state store is unavailable")
	}
	if req.SlotID == "" || req.RepositoryID == "" || req.WorktreePath == "" || req.CommonDir == "" {
		return WorktreeOwnership{}, ownershipFailure("slot, repository, worktree path, and common directory are required")
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
	var workspaceRoot, mainWorktreePath, commonDir, slotPath, worktreePath string
	var workspaceID, slotState, repositoryID, repositoryState, relativePath string
	err = tx.QueryRowContext(ctx, `
		SELECT sl.id,sl.workspace_id,sl.generation,sl.path,sl.state,
		       sr.repository_id,sr.worktree_path,sr.state,
		       w.root_path,r.main_worktree_path,r.common_git_dir,wr.relative_path
		FROM slots sl
		JOIN slot_repositories sr ON sr.slot_id=sl.id
		JOIN workspaces w ON w.id=sl.workspace_id
		JOIN repositories r ON r.id=sr.repository_id
		JOIN workspace_repositories wr ON wr.workspace_id=sl.workspace_id
		                          AND wr.repository_id=sr.repository_id
		WHERE sl.id=? AND sr.repository_id=?`, req.SlotID, req.RepositoryID).
		Scan(&out.SlotID, &workspaceID, &out.Generation, &slotPath, &slotState,
			&repositoryID, &worktreePath, &repositoryState, &workspaceRoot,
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

	canonicalSlot, err := canonicalOwnershipPath(slotPath)
	if err != nil {
		return WorktreeOwnership{}, ownershipFailure(fmt.Sprintf("recorded slot path: %v", err))
	}
	canonicalWorktree, err := canonicalOwnershipPath(worktreePath)
	if err != nil {
		return WorktreeOwnership{}, ownershipFailure(fmt.Sprintf("recorded worktree path: %v", err))
	}
	requestedWorktree, err := canonicalOwnershipPath(req.WorktreePath)
	if err != nil {
		return WorktreeOwnership{}, ownershipFailure(fmt.Sprintf("requested worktree path: %v", err))
	}
	if canonicalWorktree != requestedWorktree {
		return WorktreeOwnership{}, ownershipFailure("requested worktree path does not match the SQLite slot path")
	}
	if req.SlotPath != "" {
		requestedSlot, err := canonicalOwnershipPath(req.SlotPath)
		if err != nil {
			return WorktreeOwnership{}, ownershipFailure(fmt.Sprintf("requested slot path: %v", err))
		}
		if requestedSlot != canonicalSlot {
			return WorktreeOwnership{}, ownershipFailure("requested slot path does not match the SQLite slot path")
		}
	}
	if filepath.Clean(canonicalSlot) != filepath.Clean(canonicalWorktree) && !domain.IsWithin(canonicalSlot, canonicalWorktree) {
		return WorktreeOwnership{}, ownershipFailure("SQLite worktree path is outside its slot path")
	}
	cleanRelativePath := filepath.Clean(relativePath)
	expectedWorktree, err := canonicalOwnershipPath(filepath.Join(canonicalSlot, cleanRelativePath))
	if err != nil {
		return WorktreeOwnership{}, ownershipFailure(fmt.Sprintf("expected workspace-relative worktree path: %v", err))
	}
	if canonicalWorktree != expectedWorktree {
		return WorktreeOwnership{}, ownershipFailure("SQLite worktree path does not match the workspace repository relative path")
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
	out.SlotPath = canonicalSlot
	out.WorktreePath = canonicalWorktree
	out.WorkspaceRoot = canonicalWorkspaceRoot
	out.MainWorktreePath = canonicalMain
	out.CommonDir = canonicalCommon
	out.RelativePath = cleanRelativePath
	out.SlotState = slotState
	out.RepositoryState = repositoryState
	return out, nil
}

// ValidateSlotOwnership is used before deleting or recreating a slot root. It
// covers slots that have no repository worktree (and therefore cannot be
// proven through ValidateWorktreeOwnership) while retaining the same
// fail-closed path/state checks.
func (s *Store) ValidateSlotOwnership(ctx context.Context, req SlotOwnershipRequest) error {
	if s == nil || s.db == nil {
		return ownershipFailure("state store is unavailable")
	}
	if req.SlotID == "" || req.Path == "" || len(req.AllowedSlotStates) == 0 {
		return ownershipFailure("slot, path, and eligible states are required")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ownershipDatabaseFailure(err)
	}
	defer func() { _ = tx.Rollback() }()
	var workspaceID, slotState, rawPath, rawRoot string
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(sl.workspace_id,''),sl.path,sl.state,w.root_path
		FROM slots sl LEFT JOIN workspaces w ON w.id=sl.workspace_id
		WHERE sl.id=?`, req.SlotID).
		Scan(&workspaceID, &rawPath, &slotState, &rawRoot)
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
	canonicalPath, err := canonicalOwnershipPath(rawPath)
	if err != nil {
		return ownershipFailure(fmt.Sprintf("recorded slot path: %v", err))
	}
	requestedPath, err := canonicalOwnershipPath(req.Path)
	if err != nil {
		return ownershipFailure(fmt.Sprintf("requested slot path: %v", err))
	}
	if canonicalPath != requestedPath {
		return ownershipFailure("requested slot path does not match the SQLite slot path")
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

// canonicalOwnershipPath resolves every existing component and appends a
// missing suffix lexically. Prepare and interrupted removal both legitimately
// validate a missing worktree leaf, but no existing symlink component is ever
// accepted.
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
