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

type ownershipMarker struct {
	Version   int    `json:"version"`
	SlotID    string `json:"slot_id"`
	Target    string `json:"target"`
	CommonDir string `json:"common_dir"`
}

// EnsureOwnershipMarker creates the wx ownership proof outside the worktree
// itself. A marker is only accepted when it binds the slot id, physical target,
// and Git common directory that wx is about to use.
func EnsureOwnershipMarker(root, target, slotID, commonDir string) error {
	if slotID == "" || strings.ContainsAny(slotID, `/\`) {
		return errors.New("invalid wx ownership slot id")
	}
	marker, err := newOwnershipMarker(target, slotID, commonDir, true)
	if err != nil {
		return err
	}
	owner, markerRelative, err := openMarkerRoot(root, target)
	if err != nil {
		return err
	}
	defer func() { _ = owner.Close() }()
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

// ValidateOwnershipMarker verifies a marker for an existing target and the
// expected slot/common directory. It intentionally rejects a missing target;
// removal has a separate helper because a registered worktree can be missing
// after an interrupted physical deletion.
func ValidateOwnershipMarker(root, target, slotID, commonDir string) error {
	if strings.ContainsAny(slotID, `/\`) {
		return markerOwnershipFailure(errors.New("invalid wx ownership slot id"))
	}
	marker, err := newOwnershipMarker(target, slotID, commonDir, false)
	if err != nil {
		return markerOwnershipFailure(err)
	}
	owner, markerRelative, err := openMarkerRoot(root, target)
	if err != nil {
		return markerOwnershipFailure(err)
	}
	defer func() { _ = owner.Close() }()
	actual, err := readOwnershipMarker(owner, markerRelative)
	if err != nil {
		return markerOwnershipFailure(err)
	}
	if actual.Target != marker.Target || actual.CommonDir != marker.CommonDir {
		return markerOwnershipFailure(errors.New("wx ownership marker does not match expected worktree"))
	}
	if slotID != "" && actual.SlotID != slotID {
		return markerOwnershipFailure(errors.New("wx ownership marker does not match expected slot"))
	}
	return nil
}

// ValidateRemovalOwnership verifies the marker even when the physical
// worktree leaf is missing and returns the slot id encoded by the marker.
func ValidateRemovalOwnership(root, target, commonDir string) (string, error) {
	marker, err := newOwnershipMarker(target, "", commonDir, true)
	if err != nil {
		return "", markerOwnershipFailure(err)
	}
	owner, markerRelative, err := openMarkerRoot(root, target)
	if err != nil {
		return "", markerOwnershipFailure(err)
	}
	defer func() { _ = owner.Close() }()
	actual, err := readOwnershipMarker(owner, markerRelative)
	if err != nil {
		return "", markerOwnershipFailure(err)
	}
	if actual.Target != marker.Target || actual.CommonDir != marker.CommonDir {
		if actual.Target == marker.Target && actual.CommonDir != marker.CommonDir {
			return "", markerOwnershipFailure(errors.New("wx ownership marker common directory does not match recorded worktree"))
		}
		return "", markerOwnershipFailure(errors.New("wx ownership marker does not match recorded worktree"))
	}
	return actual.SlotID, nil
}

func markerOwnershipFailure(err error) error {
	if err == nil || errors.Is(err, state.ErrOwnership) {
		return err
	}
	return fmt.Errorf("%w: %v", state.ErrOwnership, err)
}

func newOwnershipMarker(target, slotID, commonDir string, allowMissingTarget bool) (ownershipMarker, error) {
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
		absoluteTarget, err = filepath.EvalSymlinks(absoluteTarget)
		if err != nil {
			return ownershipMarker{}, err
		}
		absoluteTarget = filepath.Clean(absoluteTarget)
	} else if !allowMissingTarget || !errors.Is(statErr, os.ErrNotExist) {
		return ownershipMarker{}, statErr
	}
	absoluteCommon, err := filepath.Abs(filepath.Clean(commonDir))
	if err != nil {
		return ownershipMarker{}, err
	}
	absoluteCommon, err = filepath.EvalSymlinks(absoluteCommon)
	if err != nil {
		return ownershipMarker{}, fmt.Errorf("canonicalize Git common directory: %w", err)
	}
	return ownershipMarker{Version: 1, SlotID: slotID, Target: filepath.Clean(absoluteTarget), CommonDir: filepath.Clean(absoluteCommon)}, nil
}

func openMarkerRoot(root, target string) (*os.Root, string, error) {
	absoluteRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return nil, "", err
	}
	absoluteTarget, err := filepath.Abs(filepath.Clean(target))
	if err != nil {
		return nil, "", err
	}
	if !domain.IsWithin(absoluteRoot, absoluteTarget) {
		return nil, "", errors.New("worktree target is outside wx ownership root")
	}
	marker := filepath.Join(ownershipMarkerBase(absoluteRoot, absoluteTarget), ownershipMarkerNameForTarget(absoluteTarget))
	if !domain.IsWithin(absoluteRoot, marker) {
		return nil, "", errors.New("wx ownership marker is outside ownership root")
	}
	owner, relative, err := domain.OpenOwnedRoot(absoluteRoot, marker)
	if err != nil {
		return nil, "", err
	}
	return owner, relative, nil
}

func ownershipMarkerNameForTarget(target string) string {
	return ownershipMarkerPrefix + domain.StableID("worktree", filepath.Clean(target))
}

func ownershipMarkerBase(root, target string) string {
	relative, err := filepath.Rel(root, target)
	if err != nil {
		return filepath.Dir(target)
	}
	parts := strings.Split(relative, string(filepath.Separator))
	for index := 0; index+2 < len(parts); index++ {
		if (parts[index] == "slots" || parts[index] == "unbound") && parts[index+1] != "" && parts[index+2] == "root" {
			base := root
			for _, part := range parts[:index+2] {
				base = filepath.Join(base, part)
			}
			return base
		}
	}
	return filepath.Dir(target)
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
	if marker.Version != 1 || marker.SlotID == "" || strings.ContainsAny(marker.SlotID, `/\`) || marker.Target == "" || marker.CommonDir == "" {
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

type worktreeRecord struct {
	Path       string
	LockReason string
	Locked     bool
}

// ValidateRegisteredWorktree verifies that Git still points at the physical
// target. When required, the lock reason must bind the target to the same wx
// slot that owns its marker.
func ValidateRegisteredWorktree(ctx context.Context, runner *gitx.Runner, mainPath, target, slotID string, requireLock bool) error {
	reason, found, err := RegisteredWorktreeLockReason(ctx, runner, mainPath, target)
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
	if reason != "wx:"+slotID+":READY" && reason != "wx:"+slotID+":PREPARING" && reason != "wx:"+slotID+":RESTORING" {
		return fmt.Errorf("worktree lock reason does not belong to wx slot %s", slotID)
	}
	return nil
}

// RegisteredWorktreeLockReason returns the Git lock reason for target. The
// found result is false when Git has no registration at that path, including
// the idempotent post-removal case.
func RegisteredWorktreeLockReason(ctx context.Context, runner *gitx.Runner, mainPath, target string) (reason string, found bool, err error) {
	reason, _, found, err = RegisteredWorktreeLockStatus(ctx, runner, mainPath, target)
	return reason, found, err
}

// RegisteredWorktreeLockStatus is the strict form of
// RegisteredWorktreeLockReason. It distinguishes an unlocked worktree from a
// lock with an empty reason, which matters during the remove handoff.
func RegisteredWorktreeLockStatus(ctx context.Context, runner *gitx.Runner, mainPath, target string) (reason string, locked, found bool, err error) {
	listed, err := runner.Run(ctx, mainPath, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return "", false, false, err
	}
	want, err := canonicalPathAllowMissing(target)
	if err != nil {
		return "", false, false, err
	}
	for _, record := range parseWorktreeRecords(listed.Stdout) {
		if err := validatePhysicalPathAllowMissingLeaf(record.Path); err != nil {
			// A Git registration reached through a symlink alias is not an
			// ownership match. Resolving it first would hide a path replacement.
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

func parseWorktreeRecords(output string) []worktreeRecord {
	var records []worktreeRecord
	var current *worktreeRecord
	for _, field := range strings.Split(output, "\x00") {
		if strings.HasPrefix(field, "worktree ") {
			if current != nil {
				records = append(records, *current)
			}
			current = &worktreeRecord{Path: strings.TrimPrefix(field, "worktree ")}
			continue
		}
		if current == nil {
			continue
		}
		if field == "locked" {
			current.Locked = true
			current.LockReason = ""
		} else if strings.HasPrefix(field, "locked ") {
			current.Locked = true
			current.LockReason = strings.TrimPrefix(field, "locked ")
		}
	}
	if current != nil {
		records = append(records, *current)
	}
	return records
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
