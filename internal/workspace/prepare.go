package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
)

type Preparer struct {
	Git    *gitx.Runner
	Config config.Config
}

func (p *Preparer) Prepare(ctx context.Context, repo discovery.Repository, target, oid, slotID string) error {
	return p.prepare(ctx, repo, target, oid, slotID, preparePhaseCreate)
}

// PrepareForRestore creates the clean base of a restore while leaving the
// worktree locked as RESTORING. Snapshot contents and the saved index must be
// installed before any resume-phase prepare command runs.
func (p *Preparer) PrepareForRestore(ctx context.Context, repo discovery.Repository, target, oid, slotID string) error {
	return p.prepare(ctx, repo, target, oid, slotID, preparePhaseRestore)
}

type preparePhase string

const (
	preparePhaseCreate  preparePhase = "create"
	preparePhaseRestore preparePhase = "restore"
)

func (p *Preparer) prepare(ctx context.Context, repo discovery.Repository, target, oid, slotID string, phase preparePhase) error {
	root, err := config.ExpandHome(p.Config.Storage.WorktreeRoot)
	if err != nil {
		return err
	}
	if err := ensurePhysicalRoot(root); err != nil {
		return err
	}
	target = filepath.Clean(target)
	if !domain.IsWithin(root, target) {
		return fmt.Errorf("target %s is outside wx worktree root", target)
	}
	ownedRoot, relativeTarget, err := domain.OpenOwnedRoot(root, target)
	if err != nil {
		return fmt.Errorf("open wx worktree root: %w", err)
	}
	defer func() { _ = ownedRoot.Close() }()
	if err := ownedRoot.MkdirAll(filepath.Dir(relativeTarget), 0o700); err != nil {
		return fmt.Errorf("create worktree parent safely: %w", err)
	}
	return p.Git.WithCommonDirLock(string(repo.CommonDir), func() error {
		// Re-open the descriptor after taking the common-directory lock and
		// repeat the physical/ownership checks. A pre-lock check alone would
		// allow a path replacement between validation and the Git operation.
		lockedRoot, lockedRelativeTarget, err := domain.OpenOwnedRoot(root, target)
		if err != nil {
			return fmt.Errorf("revalidate wx worktree root: %w", err)
		}
		defer func() { _ = lockedRoot.Close() }()
		if err := lockedRoot.MkdirAll(filepath.Dir(lockedRelativeTarget), 0o700); err != nil {
			return fmt.Errorf("create worktree parent safely: %w", err)
		}
		if _, err := lockedRoot.Lstat(lockedRelativeTarget); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		existingWorktree, err := p.existingTargetState(ctx, repo, target, oid, slotID, root, lockedRoot, lockedRelativeTarget)
		if err != nil {
			return err
		}
		if err := EnsureOwnershipMarker(root, target, slotID, string(repo.CommonDir)); err != nil {
			return fmt.Errorf("prepare wx ownership marker: %w", err)
		}
		if !existingWorktree {
			if err := domain.ValidatePhysicalPath(filepath.Dir(target), false); err != nil {
				return fmt.Errorf("worktree parent contains a symlink: %w", err)
			}
			if _, err := lockedRoot.Lstat(filepath.Dir(lockedRelativeTarget)); err != nil {
				return fmt.Errorf("worktree parent ownership changed: %w", err)
			}
			if _, err := p.Git.Run(ctx, string(repo.MainPath), "worktree", "add", "--detach", target, oid); err != nil {
				return err
			}
			if _, err := lockedRoot.Lstat(lockedRelativeTarget); err != nil {
				return fmt.Errorf("prepared worktree escaped ownership root: %w", err)
			}
		}
		cleanup := !existingWorktree
		ownedAfterLock := false
		defer func() {
			if cleanup && ownedAfterLock {
				// A failed preparation is removable only while ownership can still
				// be proved. If a command or concurrent filesystem change invalidated
				// that proof, leave the locked target and marker for quarantine/reconcile.
				if err := p.validateExistingWorktreeOwned(context.Background(), repo, target, oid, slotID); err != nil {
					return
				}
				if _, err := p.Git.Run(context.Background(), string(repo.MainPath), "worktree", "unlock", target); err != nil {
					return
				}
				if _, err := p.Git.Run(context.Background(), string(repo.MainPath), "worktree", "remove", "--force", target); err != nil {
					return
				}
				_ = removeOwnershipMarker(root, target)
			}
		}()
		if existingWorktree {
			if _, err := p.Git.Run(ctx, string(repo.MainPath), "worktree", "unlock", target); err != nil {
				return fmt.Errorf("unlock existing wx worktree: %w", err)
			}
		}
		lockState := "PREPARING"
		if phase == preparePhaseRestore {
			lockState = "RESTORING"
		}
		if _, err := p.Git.Run(ctx, string(repo.MainPath), "worktree", "lock", "--reason", "wx:"+slotID+":"+lockState, target); err != nil {
			return err
		}
		// This validation is deliberately after the new lock is acquired. It
		// proves that the marker, physical path, Git registration, OID, and
		// lock reason still describe the same slot before any file operation.
		if err := p.validateExistingWorktreeOwned(ctx, repo, target, oid, slotID); err != nil {
			return fmt.Errorf("wx worktree ownership changed after lock: %w", err)
		}
		ownedAfterLock = true
		if err := p.copyIncludes(repo, target); err != nil {
			return err
		}
		if err := p.createLinks(ctx, repo, target); err != nil {
			return err
		}
		if phase == preparePhaseCreate {
			if err := p.runPrepare(ctx, repo, target); err != nil {
				return err
			}
		}
		if phase == preparePhaseCreate {
			if err := p.validateTrackedClean(ctx, target); err != nil {
				return err
			}
		}
		head, err := p.Git.Run(ctx, target, "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		if strings.TrimSpace(head.Stdout) != oid {
			return fmt.Errorf("prepared HEAD differs from requested OID")
		}
		if !gitx.IsDetached(ctx, p.Git, target) {
			return errors.New("prepared worktree is not detached")
		}
		if phase == preparePhaseRestore {
			// Keep the RESTORING lock until archive.Manager has restored the
			// snapshot tree/index and run the resume-phase command.
			cleanup = false
			return nil
		}
		if _, err = p.Git.Run(ctx, string(repo.MainPath), "worktree", "unlock", target); err != nil {
			return err
		}
		_, err = p.Git.Run(ctx, string(repo.MainPath), "worktree", "lock", "--reason", "wx:"+slotID+":READY", target)
		if err != nil {
			return err
		}
		if err := p.validateExistingWorktreeOwned(ctx, repo, target, oid, slotID); err != nil {
			return fmt.Errorf("wx worktree ownership changed before READY: %w", err)
		}
		cleanup = false
		return nil
	})
}

// PrepareResume runs only the resume phase of a repository prepare. The
// caller invokes it after restoring the snapshot tree and index, so commands
// can inspect the recovered files and their staged state.
func (p *Preparer) PrepareResume(ctx context.Context, repo discovery.Repository, target, oid, slotID string) error {
	if err := p.validateExistingWorktreeOwned(ctx, repo, target, oid, slotID); err != nil {
		return fmt.Errorf("validate restoring worktree before resume prepare: %w", err)
	}
	if err := p.runPrepare(ctx, repo, target); err != nil {
		return err
	}
	if err := p.validateExistingWorktreeOwned(ctx, repo, target, oid, slotID); err != nil {
		return fmt.Errorf("wx worktree ownership changed during resume prepare: %w", err)
	}
	return nil
}

// FinishRestore transitions a successfully restored repository from its
// RESTORING lock to the READY lock. The final ownership check occurs while the
// restore lock is still held and again after the READY lock is acquired.
func (p *Preparer) FinishRestore(ctx context.Context, repo discovery.Repository, target, oid, slotID string) error {
	if err := p.validateExistingWorktreeOwned(ctx, repo, target, oid, slotID); err != nil {
		return err
	}
	if _, err := p.Git.Run(ctx, string(repo.MainPath), "worktree", "unlock", target); err != nil {
		return fmt.Errorf("unlock restored worktree: %w", err)
	}
	if _, err := p.Git.Run(ctx, string(repo.MainPath), "worktree", "lock", "--reason", "wx:"+slotID+":READY", target); err != nil {
		return err
	}
	if err := p.validateExistingWorktreeOwned(ctx, repo, target, oid, slotID); err != nil {
		return fmt.Errorf("validate restored READY worktree: %w", err)
	}
	return nil
}

func (p *Preparer) existingTargetState(ctx context.Context, repo discovery.Repository, target, oid, slotID, root string, ownedRoot *os.Root, relativeTarget string) (bool, error) {
	info, err := ownedRoot.Lstat(relativeTarget)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, errors.New("target is not a physical directory")
	}
	entries, err := readOwnedDirectory(ownedRoot, relativeTarget)
	if err != nil {
		return false, err
	}
	if len(entries) == 0 {
		// An empty shell may be left by an interrupted allocation. A marker,
		// when present, still has to match; a missing marker is created below.
		if _, markerErr := os.Stat(filepath.Join(ownershipMarkerBase(root, target), ownershipMarkerNameForTarget(target))); markerErr != nil && !errors.Is(markerErr, os.ErrNotExist) {
			return false, markerErr
		}
		return false, nil
	}
	if err := p.validateExistingWorktreeOwned(ctx, repo, target, oid, slotID); err != nil {
		return false, fmt.Errorf("non-empty target is not the expected worktree: %w", err)
	}
	return true, nil
}

func readOwnedDirectory(root *os.Root, relative string) ([]string, error) {
	directory, err := root.Open(relative)
	if err != nil {
		return nil, err
	}
	defer func() { _ = directory.Close() }()
	return directory.Readdirnames(-1)
}

func ensurePhysicalRoot(path string) error {
	if err := domain.EnsurePhysicalDirectory(path, 0o700); err != nil {
		return fmt.Errorf("create wx worktree root safely: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("wx worktree root is not a physical directory")
	}
	if err := domain.ValidatePhysicalPath(path, false); err != nil {
		return err
	}
	return nil
}

func (p *Preparer) validateExistingWorktree(ctx context.Context, repo discovery.Repository, target, oid string) error {
	return p.validateExistingWorktreeOwned(ctx, repo, target, oid, "")
}

func (p *Preparer) validateExistingWorktreeOwned(ctx context.Context, repo discovery.Repository, target, oid, slotID string) error {
	root, err := config.ExpandHome(p.Config.Storage.WorktreeRoot)
	if err != nil {
		return err
	}
	if err := domain.ValidatePhysicalPath(root, false); err != nil {
		return fmt.Errorf("wx worktree root is not physical: %w", err)
	}
	target = filepath.Clean(target)
	if !domain.IsWithin(root, target) {
		return errors.New("worktree target is outside wx ownership root")
	}
	if err := ValidateOwnershipMarker(root, target, slotID, string(repo.CommonDir)); err != nil {
		return err
	}
	if err := domain.ValidatePhysicalPath(target, false); err != nil {
		return fmt.Errorf("worktree target contains a symlink: %w", err)
	}
	gitMarker, err := os.Lstat(filepath.Join(target, ".git"))
	if err != nil || gitMarker.Mode()&os.ModeSymlink != 0 {
		return errors.New("missing or unsafe .git marker")
	}
	common, err := p.Git.Run(ctx, target, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	expectedCommon, err := filepath.EvalSymlinks(string(repo.CommonDir))
	if err != nil {
		return err
	}
	actualCommon, err := filepath.EvalSymlinks(strings.TrimSpace(common.Stdout))
	if err != nil || actualCommon != expectedCommon {
		return errors.New("common Git directory does not match")
	}
	head, err := p.Git.Run(ctx, target, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(head.Stdout) != oid || !gitx.IsDetached(ctx, p.Git, target) {
		return errors.New("HEAD is not the expected detached commit")
	}
	return ValidateRegisteredWorktree(ctx, p.Git, string(repo.MainPath), target, slotID, slotID != "")
}

// ValidateReady verifies the physical and Git-administrative invariants that
// make a stored READY worktree safe to lease.
func (p *Preparer) ValidateReady(ctx context.Context, repo discovery.Repository, target, oid string) error {
	if err := p.ValidateOwnership(ctx, repo, target, oid); err != nil {
		return err
	}
	return p.validateTrackedClean(ctx, target)
}

// ValidateOwnership verifies the physical and Git-administrative ownership
// invariants without requiring a clean index or working tree. Restored leased
// sessions intentionally contain the archived user's tracked changes.
func (p *Preparer) ValidateOwnership(ctx context.Context, repo discovery.Repository, target, oid string) error {
	if err := p.validateExistingWorktree(ctx, repo, target, oid); err != nil {
		return err
	}
	root, err := config.ExpandHome(p.Config.Storage.WorktreeRoot)
	if err != nil {
		return err
	}
	common, err := filepath.EvalSymlinks(string(repo.CommonDir))
	if err != nil {
		return err
	}
	slotID, err := ValidateRemovalOwnership(root, target, common)
	if err != nil {
		return err
	}
	return ValidateRegisteredWorktree(ctx, p.Git, string(repo.MainPath), target, slotID, true)
}

func (p *Preparer) validateTrackedClean(ctx context.Context, target string) error {
	status, err := p.Git.Run(ctx, target, "status", "--porcelain=v1", "--untracked-files=no")
	if err != nil {
		return err
	}
	if strings.TrimSpace(status.Stdout) != "" {
		return errors.New("prepared worktree has tracked changes")
	}
	return nil
}

func (p *Preparer) copyIncludes(repo discovery.Repository, target string) error {
	manifest := filepath.Join(string(repo.MainPath), ".worktreeinclude")
	if err := validateOptionalManifest(manifest); err != nil {
		return err
	}
	patterns, err := discovery.ReadPatterns(manifest)
	if err != nil {
		return err
	}
	for _, pattern := range patterns {
		clean := filepath.Clean(pattern)
		if filepath.IsAbs(pattern) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe .worktreeinclude pattern %q", pattern)
		}
		matches, err := safeGlob(string(repo.MainPath), pattern)
		if err != nil {
			return err
		}
		for _, src := range matches {
			rel, err := filepath.Rel(string(repo.MainPath), src)
			if err != nil {
				return err
			}
			rel, err = safeRelative(rel)
			if err != nil {
				return fmt.Errorf("unsafe .worktreeinclude match %q: %w", src, err)
			}
			tracked, err := p.Git.Run(context.Background(), string(repo.MainPath), "ls-files", "--error-unmatch", "--", rel)
			if err == nil && strings.TrimSpace(tracked.Stdout) != "" {
				return fmt.Errorf(".worktreeinclude would overwrite tracked path %s", rel)
			}
			if err := copyPathFromRoots(string(repo.MainPath), rel, target, rel); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *Preparer) createLinks(ctx context.Context, repo discovery.Repository, target string) error {
	manifest := filepath.Join(string(repo.MainPath), ".worktreelink")
	if err := validateOptionalManifest(manifest); err != nil {
		return err
	}
	patterns, err := discovery.ReadPatterns(manifest)
	if err != nil {
		return err
	}
	for _, rel := range patterns {
		clean := filepath.Clean(rel)
		if filepath.IsAbs(rel) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe .worktreelink path %q", rel)
		}
		if _, err := p.Git.Run(ctx, string(repo.MainPath), "check-ignore", "-q", "--", clean); err != nil {
			return fmt.Errorf(".worktreelink path %q is not ignored", rel)
		}
		source := filepath.Join(string(repo.MainPath), clean)
		if err := domain.ValidatePhysicalPath(source, false); err != nil {
			return fmt.Errorf(".worktreelink source %s is not physical: %w", rel, err)
		}
		if err := domain.ValidatePhysicalPath(target, false); err != nil {
			return fmt.Errorf(".worktreelink target is not physical: %w", err)
		}
		destinationRoot, err := os.OpenRoot(target)
		if err != nil {
			return err
		}
		destinationRelative, err := safeRelative(clean)
		if err != nil {
			_ = destinationRoot.Close()
			return err
		}
		if err := ensureRootDirectory(destinationRoot, filepath.Dir(destinationRelative)); err != nil {
			_ = destinationRoot.Close()
			return err
		}
		if info, err := destinationRoot.Lstat(destinationRelative); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				existing, readErr := destinationRoot.Readlink(destinationRelative)
				if readErr == nil && existing == source {
					_ = destinationRoot.Close()
					continue
				}
			}
			_ = destinationRoot.Close()
			return fmt.Errorf(".worktreelink target collision %s", rel)
		} else if !errors.Is(err, os.ErrNotExist) {
			_ = destinationRoot.Close()
			return err
		}
		if err := destinationRoot.Symlink(source, destinationRelative); err != nil {
			_ = destinationRoot.Close()
			return err
		}
		if err := destinationRoot.Close(); err != nil {
			return err
		}
	}
	return nil
}

func (p *Preparer) runPrepare(ctx context.Context, repo discovery.Repository, target string) error {
	override, ok := p.Config.Repositories[string(repo.MainPath)]
	if !ok || len(override.Prepare.Command) == 0 {
		return nil
	}
	timeout := override.Prepare.Timeout.Duration
	if timeout <= 0 {
		timeout = p.Config.Readiness.Timeout.Duration
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, override.Prepare.Command[0], override.Prepare.Command[1:]...)
	cmd.Dir = target
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run()
}

func Fingerprint(generation int, oid string, repo discovery.Repository, c config.Config) (string, error) {
	if err := domain.ValidatePhysicalPath(string(repo.MainPath), false); err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "schema=2\ngeneration=%d\noid=%s\n", generation, oid)
	for _, name := range []string{".worktreeinclude", ".worktreelink"} {
		manifest := filepath.Join(string(repo.MainPath), name)
		if err := validateOptionalManifest(manifest); err != nil {
			return "", err
		}
		data, err := os.ReadFile(manifest)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		_, _ = fmt.Fprintf(h, "manifest=%s:%x\n", name, sha256.Sum256(data))
	}
	patterns, err := discovery.ReadPatterns(filepath.Join(string(repo.MainPath), ".worktreeinclude"))
	if err != nil {
		return "", err
	}
	seenIncludes := map[string]bool{}
	for _, pattern := range patterns {
		clean := filepath.Clean(pattern)
		if filepath.IsAbs(pattern) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("unsafe .worktreeinclude pattern %q", pattern)
		}
		matches, err := safeGlob(string(repo.MainPath), pattern)
		if err != nil {
			return "", err
		}
		for _, match := range matches {
			rel, err := filepath.Rel(string(repo.MainPath), match)
			if err != nil {
				return "", err
			}
			rel, err = safeRelative(rel)
			if err != nil {
				return "", err
			}
			if seenIncludes[rel] {
				continue
			}
			seenIncludes[rel] = true
			if err := fingerprintPath(h, string(repo.MainPath), match); err != nil {
				return "", err
			}
		}
	}
	workspaceRoot, err := repositoryWorkspaceRoot(repo)
	if err != nil {
		return "", err
	}
	rules := c.Workspaces[workspaceRoot]
	_, _ = fmt.Fprintf(h, "workspace-root=%s\ncopy-rules=%q\nlink-rules=%q\n", workspaceRoot, rules.Copy, rules.Link)
	copyNames := append([]string{"AGENTS.md", "AGENTS.local.md", "CLAUDE.md", "CLAUDE.local.md"}, rules.Copy...)
	seenCopies := map[string]bool{}
	for _, name := range copyNames {
		clean, err := safeRelative(name)
		if err != nil {
			return "", err
		}
		if seenCopies[clean] {
			continue
		}
		seenCopies[clean] = true
		path := filepath.Join(workspaceRoot, clean)
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return "", err
		}
		if err := fingerprintPath(h, workspaceRoot, path); err != nil {
			return "", err
		}
	}
	for _, name := range rules.Link {
		clean, err := safeRelative(name)
		if err != nil {
			return "", err
		}
		path := filepath.Join(workspaceRoot, clean)
		if err := domain.ValidatePhysicalPath(path, false); err != nil {
			return "", err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(h, "workspace-link=%s:%s\n", clean, info.Mode())
	}
	if o, ok := c.Repositories[string(repo.MainPath)]; ok {
		_, _ = fmt.Fprint(h, o.Prepare.Command, o.Prepare.Version)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func repositoryWorkspaceRoot(repo discovery.Repository) (string, error) {
	if repo.RelativePath == "" || filepath.Clean(repo.RelativePath) == "." {
		return string(repo.MainPath), nil
	}
	rel, err := safeRelative(repo.RelativePath)
	if err != nil {
		return "", err
	}
	root := string(repo.MainPath)
	for range strings.Split(rel, string(filepath.Separator)) {
		root = filepath.Dir(root)
	}
	return root, nil
}

func fingerprintPath(h hash.Hash, root, path string) error {
	if err := domain.ValidatePhysicalPath(path, false); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return err
	}
	rel, err = safeRelative(rel)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("include symlinks are not followed")
	}
	_, _ = fmt.Fprintf(h, "path=%s mode=%s size=%d\n", rel, info.Mode(), info.Size())
	if info.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := fingerprintPath(h, root, filepath.Join(path, entry.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(h, file)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func MaterializeRoot(source, target string, rules config.Workspace) error {
	if err := domain.ValidatePhysicalPath(source, false); err != nil {
		return fmt.Errorf("workspace source is not physical: %w", err)
	}
	if err := domain.EnsurePhysicalDirectory(target, 0o700); err != nil {
		return fmt.Errorf("workspace target is not physical: %w", err)
	}
	copyNames := append([]string{"AGENTS.md", "AGENTS.local.md", "CLAUDE.md", "CLAUDE.local.md"}, rules.Copy...)
	seen := map[string]bool{}
	for _, rel := range copyNames {
		clean, err := safeRelative(rel)
		if err != nil {
			return err
		}
		if seen[clean] {
			continue
		}
		seen[clean] = true
		src := filepath.Join(source, clean)
		if _, err := os.Lstat(src); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if err := copyPathFromRoots(source, clean, target, clean); err != nil {
			return fmt.Errorf("copy workspace root path %s: %w", clean, err)
		}
	}
	for _, rel := range rules.Link {
		clean, err := safeRelative(rel)
		if err != nil {
			return err
		}
		src := filepath.Join(source, clean)
		if _, err := os.Lstat(src); err != nil {
			return fmt.Errorf("link workspace root path %s: %w", clean, err)
		}
		if err := domain.ValidatePhysicalPath(src, false); err != nil {
			return fmt.Errorf("workspace link source %s is not physical: %w", clean, err)
		}
		destinationRoot, err := os.OpenRoot(target)
		if err != nil {
			return err
		}
		if err := ensureRootDirectory(destinationRoot, filepath.Dir(clean)); err != nil {
			_ = destinationRoot.Close()
			return err
		}
		if info, err := destinationRoot.Lstat(clean); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				existing, readErr := destinationRoot.Readlink(clean)
				if readErr == nil && existing == src {
					_ = destinationRoot.Close()
					continue
				}
			}
			_ = destinationRoot.Close()
			return fmt.Errorf("workspace root link collision %s", clean)
		} else if !errors.Is(err, os.ErrNotExist) {
			_ = destinationRoot.Close()
			return err
		}
		if err := destinationRoot.Symlink(src, clean); err != nil {
			_ = destinationRoot.Close()
			return err
		}
		if err := destinationRoot.Close(); err != nil {
			return err
		}
	}
	return nil
}

func safeRelative(path string) (string, error) {
	clean := filepath.Clean(path)
	if path == "" || filepath.IsAbs(path) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe workspace root path %q", path)
	}
	return clean, nil
}

func validateOptionalManifest(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("manifest %s is not a physical regular file", path)
	}
	if err := domain.ValidatePhysicalPath(path, false); err != nil {
		return err
	}
	return nil
}

func copyPath(src, dst string) error {
	if err := domain.ValidatePhysicalPath(src, false); err != nil {
		return err
	}
	if err := domain.EnsurePhysicalDirectory(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return copyPathFromRoots(filepath.Dir(src), filepath.Base(src), filepath.Dir(dst), filepath.Base(dst))
}
