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

// WorktreeOwnershipRequest locates a repository worktree the way SQLite
// records it: a root generation, the slot's root-relative path, the
// repository's directory name inside that slot, and the inode identity the
// caller is holding. No absolute pathname crosses this boundary, so a
// pathname replacement cannot make two different directories compare equal.
type WorktreeOwnershipRequest struct {
	SlotID       string
	WorkspaceID  string
	RepositoryID string
	RootID       string
	SlotRelPath  string
	DirName      string
	// DirIdentity is the dev:ino identity of the directory the caller has
	// open. When it is set, the recorded identity must exist and match; an
	// empty record is a failure, never a pass. It is empty only before the
	// worktree exists (the pre-creation checks inside prepare), which is the
	// one case where no identity can be presented at all.
	DirIdentity             string
	CommonDir               string
	AllowedSlotStates       []string
	AllowedRepositoryStates []string
}

// WorktreeOwnership is the durable identity proved by a successful
// ValidateWorktreeOwnership call. It reports the location as recorded rather
// than as a composed absolute path, so diagnostics show which root
// generation answered.
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
	// DirIdentity is the identity SQLite has recorded for the worktree
	// directory, empty until preparation or restore completes and records
	// one. It is returned so a caller that cannot require an identity yet -
	// the re-run of an interrupted preparation, which legitimately finds no
	// record - can still refuse a directory whose recorded identity differs.
	DirIdentity string
}

// SlotOwnershipRequest locates a slot directory for the operations that have
// no repository worktree to prove through ValidateWorktreeOwnership.
type SlotOwnershipRequest struct {
	SlotID            string
	WorkspaceID       string
	RootID            string
	RelPath           string
	DirIdentity       string
	AllowedSlotStates []string
}

var _ OwnershipValidator = (*Store)(nil)

// validateOwnershipRelative rejects anything that cannot be a root-relative
// location: an empty value, an absolute path, an unclean spelling, "." (which
// would name the root or slot directory itself), or any escape through "..".
func validateOwnershipRelative(kind, value string) error {
	if value == "" {
		return fmt.Errorf("%s is empty", kind)
	}
	if value == "." || filepath.Clean(value) != value || !filepath.IsLocal(value) {
		return fmt.Errorf("%s %q is not a safe root-relative location", kind, value)
	}
	return nil
}

// validateOwnershipComponent additionally requires exactly one path
// component. A repository's directory name must be a direct child of its
// slot: filepath.IsLocal accepts "a/b", so without this a recorded dir_name
// could place a worktree at an arbitrary depth below the slot and the
// UNIQUE(slot_id, dir_name) constraint would not notice.
func validateOwnershipComponent(kind, value string) error {
	if err := validateOwnershipRelative(kind, value); err != nil {
		return err
	}
	if strings.ContainsRune(value, filepath.Separator) {
		return fmt.Errorf("%s %q is not a single path component", kind, value)
	}
	return nil
}

// ValidateWorktreeOwnership proves that the caller's worktree is the exact
// directory recorded for the requested slot/repository pair: the same root
// generation, the same root-relative slot path, the same directory name, and
// the same inode. It also proves that the repository belongs to the slot's
// workspace, that its common Git directory is the one recorded for that
// repository, and that both durable state machines are in caller-approved
// states.
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
	// Fail closed on identity: a caller holding a descriptor must find a
	// recorded identity, and it must be the same inode. Treating a missing
	// record as a match would reinstate exactly the pathname-only proof this
	// check replaced.
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

// ValidateSlotOwnership is used before deleting or recreating a slot
// directory. It covers slots that have no repository worktree (and therefore
// cannot be proven through ValidateWorktreeOwnership) while retaining the
// same fail-closed location and state checks.
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
