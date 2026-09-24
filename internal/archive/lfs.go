package archive

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/HappyOnigiri/WorktreeX/internal/gitx"
	"github.com/HappyOnigiri/WorktreeX/internal/workspace"
)

const maxLFSPointerBlobBytes = 1 << 20

func (m *Manager) logLFSOptimizationWarning(operation string, err error) {
	if m.Preparer != nil && m.Preparer.Log != nil {
		m.Preparer.Log.Warn("LFS cache optimization skipped", "operation", operation, "error", err)
	}
}

// snapshotLFSObjects は HEAD と worktree tree の差分から、新しい LFS pointer を拾う。
// Git の tree と blob を同じ descriptor-bound command で読むため、status の再観測を行わない。
func snapshotLFSObjects(
	run func([]string, []byte, ...string) (gitx.Result, error),
	env []string,
	head, worktreeTree string,
) ([]workspace.LFSObjectCandidate, error) {
	result, err := run(env, nil, "diff-tree", "-r", "-z", "--raw", head, worktreeTree)
	if err != nil {
		return nil, fmt.Errorf("list changed LFS paths: %w", err)
	}
	changes, err := parseSnapshotTreeDiff(result.Stdout)
	if err != nil {
		return nil, err
	}
	if len(changes) == 0 {
		return nil, nil
	}
	oids := make([]string, 0, len(changes))
	seen := make(map[string]bool, len(changes))
	for _, change := range changes {
		if seen[change.newOID] {
			continue
		}
		seen[change.newOID] = true
		oids = append(oids, change.newOID)
	}
	input := strings.Join(oids, "\n") + "\n"
	blobs, err := run(env, []byte(input), "cat-file", "--batch")
	if err != nil {
		return nil, fmt.Errorf("read changed LFS pointers: %w", err)
	}
	pointers, err := parseSnapshotLFSPointerBatch(blobs.Stdout, oids)
	if err != nil {
		return nil, err
	}
	candidates := make([]workspace.LFSObjectCandidate, 0, len(changes))
	for _, change := range changes {
		pointer, ok := pointers[change.newOID]
		if !ok {
			continue
		}
		candidates = append(candidates, workspace.LFSObjectCandidate{Path: change.path, Pointer: pointer})
	}
	return candidates, nil
}

type snapshotTreeChange struct {
	newOID string
	path   string
}

func validateSnapshotTreeDiffProgress(previous, current int) error {
	if current <= previous {
		return fmt.Errorf("Git tree diff cursor did not advance: %d -> %d", previous, current)
	}
	return nil
}

func parseSnapshotTreeDiff(output string) ([]snapshotTreeChange, error) {
	parts := strings.Split(output, "\x00")
	changes := make([]snapshotTreeChange, 0, len(parts))
	for index := 0; index < len(parts); {
		record := parts[index]
		previousIndex := index
		index++
		if err := validateSnapshotTreeDiffProgress(previousIndex, index); err != nil {
			return nil, err
		}
		if record == "" {
			continue
		}
		if index >= len(parts) || parts[index] == "" {
			return nil, errors.New("invalid Git tree diff record")
		}
		path := parts[index]
		previousIndex = index
		index++
		if err := validateSnapshotTreeDiffProgress(previousIndex, index); err != nil {
			return nil, err
		}
		fields := strings.Fields(record)
		if len(fields) < 5 || !strings.HasPrefix(fields[0], ":") {
			return nil, errors.New("invalid Git tree diff header")
		}
		status := fields[4]
		if len(status) > 0 && (status[0] == 'R' || status[0] == 'C') {
			if index >= len(parts) || parts[index] == "" {
				return nil, errors.New("invalid Git tree rename record")
			}
			path = parts[index]
			previousIndex = index
			index++
			if err := validateSnapshotTreeDiffProgress(previousIndex, index); err != nil {
				return nil, err
			}
		}
		if !validSnapshotPath(path) {
			return nil, fmt.Errorf("unsafe Git tree path %q", path)
		}
		newOID := fields[3]
		if !validGitOID(newOID) || allZeroOID(newOID) {
			continue
		}
		if fields[1] != "100644" && fields[1] != "100755" {
			continue
		}
		changes = append(changes, snapshotTreeChange{newOID: newOID, path: path})
	}
	return changes, nil
}

func parseSnapshotLFSPointerBatch(output string, oids []string) (map[string]workspace.LFSPointer, error) {
	pointers := make(map[string]workspace.LFSPointer)
	reader := bufio.NewReader(strings.NewReader(output))
	for _, oid := range oids {
		header, err := reader.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("read LFS pointer header for %s: %w", oid, err)
		}
		fields := strings.Fields(strings.TrimSpace(header))
		if len(fields) == 2 && fields[1] == "missing" {
			continue
		}
		if len(fields) != 3 {
			return nil, fmt.Errorf("invalid LFS pointer header for %s", oid)
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("invalid LFS pointer blob size for %s", oid)
		}
		if size > maxLFSPointerBlobBytes {
			if _, err := io.CopyN(io.Discard, reader, size); err != nil {
				return nil, fmt.Errorf("skip oversized LFS pointer blob for %s: %w", oid, err)
			}
			if separator, err := reader.ReadByte(); err != nil || separator != '\n' {
				return nil, fmt.Errorf("invalid LFS pointer blob separator for %s", oid)
			}
			continue
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(reader, data); err != nil {
			return nil, fmt.Errorf("read LFS pointer blob for %s: %w", oid, err)
		}
		if separator, err := reader.ReadByte(); err != nil || separator != '\n' {
			return nil, fmt.Errorf("invalid LFS pointer blob separator for %s", oid)
		}
		if pointer, ok := workspace.ParseLFSPointer(data); ok {
			pointers[oid] = pointer
		}
	}
	return pointers, nil
}

func validSnapshotPath(path string) bool {
	if path == "" || filepath.IsAbs(filepath.FromSlash(path)) {
		return false
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	return clean != "." && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

func validGitOID(oid string) bool {
	if len(oid) != 40 && len(oid) != 64 {
		return false
	}
	_, err := hex.DecodeString(oid)
	return err == nil
}

func allZeroOID(oid string) bool {
	for _, value := range oid {
		if value != '0' {
			return false
		}
	}
	return true
}
