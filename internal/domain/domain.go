package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

type WorkspaceID string
type RepositoryID string
type SlotID string
type SessionID string

var idPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func StableID(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func ValidateID(id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("invalid wx id %q", id)
	}
	return nil
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

type SlotState string

const (
	SlotDiscovered   SlotState = "DISCOVERED"
	SlotPreparing    SlotState = "PREPARING"
	SlotReady        SlotState = "READY"
	SlotLeased       SlotState = "LEASED"
	SlotDraining     SlotState = "DRAINING"
	SlotSnapshotting SlotState = "SNAPSHOTTING"
	SlotSnapshotted  SlotState = "SNAPSHOTTED"
	SlotArchived     SlotState = "ARCHIVED"
	SlotUnbound      SlotState = "UNBOUND"
	SlotRestoring    SlotState = "RESTORING"
	SlotFailed       SlotState = "FAILED"
	SlotQuarantined  SlotState = "QUARANTINED"
)

type SessionState string

const (
	SessionAllocating   SessionState = "ALLOCATING"
	SessionStarting     SessionState = "STARTING"
	SessionActive       SessionState = "ACTIVE"
	SessionUnbound      SessionState = "UNBOUND"
	SessionRestoring    SessionState = "RESTORING"
	SessionReleasing    SessionState = "RELEASING"
	SessionSnapshotting SessionState = "SNAPSHOTTING"
	SessionArchived     SessionState = "ARCHIVED"
	SessionExpired      SessionState = "EXPIRED"
	SessionQuarantined  SessionState = "QUARANTINED"
)

func CanTransitionSlot(from, to SlotState) bool {
	allowed := map[SlotState][]SlotState{
		SlotDiscovered: {SlotPreparing}, SlotPreparing: {SlotReady}, SlotReady: {SlotLeased, SlotDraining},
		SlotLeased: {SlotDraining}, SlotDraining: {SlotSnapshotting}, SlotSnapshotting: {SlotSnapshotted},
		SlotSnapshotted: {SlotArchived}, SlotUnbound: {SlotRestoring}, SlotRestoring: {SlotLeased},
	}
	if to == SlotFailed || to == SlotQuarantined {
		return from != SlotArchived
	}
	for _, candidate := range allowed[from] {
		if candidate == to {
			return true
		}
	}
	return false
}
