package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"golang.org/x/sys/unix"

	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
	"github.com/HappyOnigiri/WorktreeX/internal/domain"
	"github.com/HappyOnigiri/WorktreeX/internal/gitx"
)

// LFSRepairFailureReason は source working tree から cache を直せなかった理由である。
// doctor はこの値を根拠にせず、準備ログと finding の payload へ本文を渡す。
type LFSRepairFailureReason string

const (
	LFSRepairCandidateMissing LFSRepairFailureReason = "candidate_missing"
	LFSRepairHashMismatch     LFSRepairFailureReason = "hash_mismatch"
	LFSRepairReadFailure      LFSRepairFailureReason = "read_failure"
	LFSRepairWriteFailure     LFSRepairFailureReason = "write_failure"
)

// LFSObjectDiagnostic は cache に問題がある object と source 側の候補を表す。
// CandidatePath が空でも source の path を直接変更することはない。
type LFSObjectDiagnostic struct {
	Object         LFSObjectInfo
	CandidatePath  string
	CandidatePaths []string
}

// LFSObjectDiagnostics は doctor と準備前修復が共有する軽量な診断結果である。
// 候補探索は size と metadata だけを読み、object 本文の hash は計算しない。
type LFSObjectDiagnostics struct {
	Objects []LFSObjectDiagnostic
}

// LFSRepairFailure は 1 object の修復失敗を、他 object の修復結果と分けて返す。
type LFSRepairFailure struct {
	Object     LFSObjectInfo
	SourcePath string
	Reason     LFSRepairFailureReason
	Err        error
}

func (f LFSRepairFailure) Error() string {
	message := string(f.Reason)
	if f.Err != nil {
		message += ": " + f.Err.Error()
	}
	if f.SourcePath != "" {
		return fmt.Sprintf("LFS object %s from %s: %s", f.Object.OID, f.SourcePath, message)
	}
	return fmt.Sprintf("LFS object %s: %s", f.Object.OID, message)
}

// LFSRepairResult は修復できた object と残った object を分離して持つ。
type LFSRepairResult struct {
	Repaired   []LFSObjectInfo
	Unresolved []LFSRepairFailure
}

// lfsRepairLocks は daemon 以外の caller が Preparer を組み立てた場合の既定 lock である。
var lfsRepairLocks gitx.KeyedLocks

func lfsRepairLocksFor(p *Preparer) *gitx.KeyedLocks {
	if p != nil && p.LFSLocks != nil {
		return p.LFSLocks
	}
	return &lfsRepairLocks
}

var lfsTemporarySequence atomic.Uint64

// DiagnoseLFSObjects は cache 欠落・破損 object と source working tree の
// size 一致候補を診断する。source は pin した descriptor から読み、本文は読まない。
func DiagnoseLFSObjects(repo discovery.Repository, objects []LFSObjectInfo) (LFSObjectDiagnostics, error) {
	if len(objects) == 0 {
		return LFSObjectDiagnostics{}, nil
	}
	if strings.TrimSpace(string(repo.MainPath)) == "" {
		return LFSObjectDiagnostics{}, errors.New("LFS diagnosis requires a source repository")
	}
	source, err := openPinnedRepositoryRoot(string(repo.MainPath))
	if err != nil {
		return LFSObjectDiagnostics{}, fmt.Errorf("open source repository for LFS diagnosis: %w", err)
	}
	defer func() { _ = source.Close() }()
	return diagnoseLFSObjectsAt(source, objects), nil
}

func diagnoseLFSObjectsAt(source *os.Root, objects []LFSObjectInfo) LFSObjectDiagnostics {
	result := LFSObjectDiagnostics{Objects: make([]LFSObjectDiagnostic, 0, len(objects))}
	for _, object := range objects {
		if !lfsObjectNeedsRepair(object) {
			continue
		}
		candidates := findLFSRepairCandidates(source, object)
		candidate := ""
		if len(candidates) > 0 {
			candidate = candidates[0]
		}
		result.Objects = append(result.Objects, LFSObjectDiagnostic{Object: object, CandidatePath: candidate, CandidatePaths: candidates})
	}
	return result
}

func lfsObjectNeedsRepair(object LFSObjectInfo) bool {
	switch object.CacheState {
	case LFSCacheHealthy:
		return false
	case LFSCacheMissing, LFSCacheCorrupt:
		return true
	}
	return !object.Cached || object.CacheSize != object.Size
}

func findLFSRepairCandidates(source *os.Root, object LFSObjectInfo) []string {
	candidates := make([]string, 0, len(object.Paths))
	seen := make(map[string]bool, len(object.Paths))
	for _, path := range object.Paths {
		clean, err := safeRelative(path)
		if err != nil {
			continue
		}
		if seen[clean] {
			continue
		}
		seen[clean] = true
		info, err := domain.PhysicalPathInfo(source, clean)
		if err != nil || !info.Mode().IsRegular() || info.Size() != object.Size {
			continue
		}
		candidates = append(candidates, clean)
	}
	return candidates
}

// RepairLFSObjects は欠落・破損 object を source working tree の実体から
// hash 検証し、common directory の cache へ原子的に install する。source の
// tracked file と Git metadata は変更せず、修復不能 object は結果へ残す。
func (p *Preparer) RepairLFSObjects(ctx context.Context, repo discovery.Repository, objects []LFSObjectInfo) (LFSRepairResult, error) {
	if len(objects) == 0 {
		return LFSRepairResult{}, nil
	}
	if strings.TrimSpace(string(repo.MainPath)) == "" || strings.TrimSpace(string(repo.CommonDir)) == "" {
		return LFSRepairResult{}, errors.New("LFS repair requires source and common directories")
	}
	locks := lfsRepairLocksFor(p)
	var result LFSRepairResult
	err := locks.With(ctx, string(repo.CommonDir), func(lockCtx context.Context) error {
		source, err := openPinnedRepositoryRoot(string(repo.MainPath))
		if err != nil {
			return fmt.Errorf("open source repository for LFS repair: %w", err)
		}
		defer func() { _ = source.Close() }()
		cache, err := OpenPhysicalRoot(string(repo.CommonDir))
		if err != nil {
			return fmt.Errorf("open LFS cache root: %w", err)
		}
		defer func() { _ = cache.Close() }()
		diagnostics := diagnoseLFSObjectsAt(source, objects)
		for _, diagnostic := range diagnostics.Objects {
			if len(diagnostic.CandidatePaths) == 0 {
				result.Unresolved = append(result.Unresolved, LFSRepairFailure{Object: diagnostic.Object, Reason: LFSRepairCandidateMissing})
				continue
			}
			var lastFailure *LFSRepairFailure
			for _, candidate := range diagnostic.CandidatePaths {
				repaired, failure := installLFSObject(source, cache, diagnostic.Object, candidate)
				if failure != nil {
					lastFailure = failure
					continue
				}
				if repaired {
					result.Repaired = append(result.Repaired, diagnostic.Object)
				}
				lastFailure = nil
				break
			}
			if lastFailure != nil {
				result.Unresolved = append(result.Unresolved, *lastFailure)
			}
		}
		return nil
	})
	return result, err
}

// lfsCacheRelativeParts は git-lfs の cache 配置を common directory からの相対で返す。
// git-lfs は OID の先頭 2 桁・次の 2 桁で directory を分け、leaf には OID 全体を使う。
// 容量推定・repair・CoW compaction が同じ object を指すよう、この組み立てだけを使う。
func lfsCacheRelativeParts(oid string) (directory, leaf string, ok bool) {
	value := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(oid)), "sha256:")
	if len(value) != 64 {
		return "", "", false
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", "", false
	}
	return filepath.Join("lfs", "objects", value[:2], value[2:4]), value, true
}

func cacheLFSRelativePath(oid string) (string, bool) {
	directory, leaf, ok := lfsCacheRelativeParts(oid)
	if !ok {
		return "", false
	}
	return filepath.Join(directory, leaf), true
}

func ensureLFSCacheDirectory(root *os.Root, relative string) error {
	clean, err := safeRelative(relative)
	if err != nil {
		return err
	}
	current := "."
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, statErr := root.Lstat(current)
		switch {
		case statErr == nil:
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("LFS cache directory %s is not a physical directory", current)
			}
		case errors.Is(statErr, os.ErrNotExist):
			if mkdirErr := root.Mkdir(current, 0o755); mkdirErr != nil {
				existing, existingErr := root.Lstat(current)
				if existingErr != nil {
					return mkdirErr
				}
				if existing.Mode()&os.ModeSymlink != 0 || !existing.IsDir() {
					return mkdirErr
				}
			}
		default:
			return statErr
		}
	}
	return nil
}

func installLFSObject(source, cache *os.Root, object LFSObjectInfo, candidate string) (bool, *LFSRepairFailure) {
	relative, ok := cacheLFSRelativePath(object.OID)
	if !ok {
		return false, &LFSRepairFailure{Object: object, SourcePath: candidate, Reason: LFSRepairWriteFailure, Err: errors.New("invalid SHA-256 object ID")}
	}
	destinationInfo, err := cache.Lstat(relative)
	switch {
	case err == nil:
		if destinationInfo.Mode()&os.ModeSymlink != 0 || !destinationInfo.Mode().IsRegular() {
			return false, &LFSRepairFailure{Object: object, SourcePath: candidate, Reason: LFSRepairWriteFailure, Err: errors.New("cache destination is not a regular file")}
		}
		if destinationInfo.Size() == object.Size {
			return false, nil
		}
	case !errors.Is(err, os.ErrNotExist):
		return false, &LFSRepairFailure{Object: object, SourcePath: candidate, Reason: LFSRepairWriteFailure, Err: err}
	}
	if err := ensureLFSCacheDirectory(cache, filepath.Dir(relative)); err != nil {
		return false, &LFSRepairFailure{Object: object, SourcePath: candidate, Reason: LFSRepairWriteFailure, Err: err}
	}
	// EnsureLFSCacheDirectory の後にも destination を確認し、別の修復が先に
	// 健全な object を置いた場合は上書きしない。
	if current, statErr := cache.Lstat(relative); statErr == nil {
		if current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() {
			return false, &LFSRepairFailure{Object: object, SourcePath: candidate, Reason: LFSRepairWriteFailure, Err: errors.New("cache destination is not a regular file")}
		}
		if current.Size() == object.Size {
			return false, nil
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return false, &LFSRepairFailure{Object: object, SourcePath: candidate, Reason: LFSRepairWriteFailure, Err: statErr}
	}

	sourceInfo, err := domain.PhysicalPathInfo(source, candidate)
	if err != nil || !sourceInfo.Mode().IsRegular() || sourceInfo.Size() != object.Size {
		if err == nil {
			err = fmt.Errorf("source size is %d, want %d", sourceInfo.Size(), object.Size)
		}
		return false, &LFSRepairFailure{Object: object, SourcePath: candidate, Reason: LFSRepairHashMismatch, Err: err}
	}
	input, err := source.OpenFile(candidate, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return false, &LFSRepairFailure{Object: object, SourcePath: candidate, Reason: LFSRepairReadFailure, Err: err}
	}
	defer func() { _ = input.Close() }()
	openedInfo, err := input.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(sourceInfo, openedInfo) {
		if err == nil {
			err = errors.New("source changed while opening")
		}
		return false, &LFSRepairFailure{Object: object, SourcePath: candidate, Reason: LFSRepairReadFailure, Err: err}
	}

	sequence := lfsTemporarySequence.Add(1)
	temporary := filepath.Join(filepath.Dir(relative), fmt.Sprintf(".wx-lfs-%d-%d", os.Getpid(), sequence))
	output, err := cache.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY|unix.O_NOFOLLOW, 0o644)
	if err != nil {
		return false, &LFSRepairFailure{Object: object, SourcePath: candidate, Reason: LFSRepairWriteFailure, Err: err}
	}
	removeTemporary := true
	defer func() {
		_ = output.Close()
		if removeTemporary {
			_ = cache.Remove(temporary)
		}
	}()
	hasher := sha256.New()
	count, copyErr := io.Copy(io.MultiWriter(output, hasher), input)
	if copyErr != nil {
		return false, &LFSRepairFailure{Object: object, SourcePath: candidate, Reason: LFSRepairReadFailure, Err: copyErr}
	}
	if count != object.Size {
		return false, &LFSRepairFailure{Object: object, SourcePath: candidate, Reason: LFSRepairHashMismatch, Err: fmt.Errorf("source size is %d after read, want %d", count, object.Size)}
	}
	wantOID := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(object.OID)), "sha256:")
	if hex.EncodeToString(hasher.Sum(nil)) != wantOID {
		return false, &LFSRepairFailure{Object: object, SourcePath: candidate, Reason: LFSRepairHashMismatch, Err: errors.New("source SHA-256 does not match pointer")}
	}
	if err := output.Sync(); err != nil {
		return false, &LFSRepairFailure{Object: object, SourcePath: candidate, Reason: LFSRepairWriteFailure, Err: err}
	}
	if err := output.Close(); err != nil {
		return false, &LFSRepairFailure{Object: object, SourcePath: candidate, Reason: LFSRepairWriteFailure, Err: err}
	}
	if err := cache.Chmod(temporary, 0o644); err != nil {
		return false, &LFSRepairFailure{Object: object, SourcePath: candidate, Reason: LFSRepairWriteFailure, Err: err}
	}
	if current, statErr := cache.Lstat(relative); statErr == nil {
		if current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() {
			return false, &LFSRepairFailure{Object: object, SourcePath: candidate, Reason: LFSRepairWriteFailure, Err: errors.New("cache destination changed to a non-regular file")}
		}
		if current.Size() == object.Size {
			return false, nil
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return false, &LFSRepairFailure{Object: object, SourcePath: candidate, Reason: LFSRepairWriteFailure, Err: statErr}
	}
	if err := cache.Rename(temporary, relative); err != nil {
		return false, &LFSRepairFailure{Object: object, SourcePath: candidate, Reason: LFSRepairWriteFailure, Err: err}
	}
	removeTemporary = false
	return true, nil
}

// VerifyLFSPathsAt は pin 済み worktree descriptor から LFS path の実体化を
// size だけで検証する。pointer 本文を hash せず、1 件でも不一致なら失敗する。
func VerifyLFSPathsAt(root *os.Root, relativeTarget string, objects []LFSObjectInfo) error {
	if root == nil {
		return errors.New("LFS integrity verification requires a worktree root")
	}
	for _, object := range objects {
		for _, path := range object.Paths {
			clean, err := safeRelative(path)
			if err != nil {
				return fmt.Errorf("LFS path %q for %s is unsafe: %w", path, object.OID, err)
			}
			relative := filepath.Join(relativeTarget, clean)
			info, err := domain.PhysicalPathInfo(root, relative)
			if err != nil {
				return fmt.Errorf("LFS path %s for %s is not materialized: %w", path, object.OID, err)
			}
			if !info.Mode().IsRegular() || info.Size() != object.Size {
				return fmt.Errorf("LFS path %s for %s has size %d, want %d", path, object.OID, info.Size(), object.Size)
			}
		}
	}
	return nil
}

func (p *Preparer) verifyPreparedLFS(root *os.Root, relativeTarget string, repo discovery.Repository) error {
	if p == nil || len(p.LFSObjects) == 0 {
		return nil
	}
	objects := p.LFSObjects[string(repo.ID)]
	if len(objects) == 0 {
		return nil
	}
	return VerifyLFSPathsAt(root, relativeTarget, objects)
}
