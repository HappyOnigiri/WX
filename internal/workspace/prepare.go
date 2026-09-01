package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
	root, err := config.ExpandHome(p.Config.Storage.WorktreeRoot)
	if err != nil {
		return err
	}
	target = filepath.Clean(target)
	if !domain.IsWithin(root, target) {
		return fmt.Errorf("target %s is outside wx worktree root", target)
	}
	existingWorktree := false
	if info, err := os.Lstat(target); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("target is a symlink")
		}
		entries, e := os.ReadDir(target)
		if e != nil {
			return e
		}
		if len(entries) > 0 {
			if err := p.validateExistingWorktree(ctx, repo, target, oid); err != nil {
				return fmt.Errorf("non-empty target is not the expected worktree: %w", err)
			}
			existingWorktree = true
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	return p.Git.WithCommonDirLock(string(repo.CommonDir), func() error {
		if !existingWorktree {
			if _, err := p.Git.Run(ctx, string(repo.MainPath), "worktree", "add", "--detach", target, oid); err != nil {
				return err
			}
		}
		cleanup := !existingWorktree
		defer func() {
			if cleanup {
				_, _ = p.Git.Run(context.Background(), string(repo.MainPath), "worktree", "remove", "--force", target)
			}
		}()
		if existingWorktree {
			_, _ = p.Git.Run(ctx, string(repo.MainPath), "worktree", "unlock", target)
		}
		if _, err := p.Git.Run(ctx, string(repo.MainPath), "worktree", "lock", "--reason", "wx:"+slotID+":PREPARING", target); err != nil {
			return err
		}
		if err := p.copyIncludes(repo, target); err != nil {
			return err
		}
		if err := p.createLinks(ctx, repo, target); err != nil {
			return err
		}
		if err := p.runPrepare(ctx, repo, target); err != nil {
			return err
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
		if _, err = p.Git.Run(ctx, string(repo.MainPath), "worktree", "unlock", target); err != nil {
			return err
		}
		_, err = p.Git.Run(ctx, string(repo.MainPath), "worktree", "lock", "--reason", "wx:"+slotID+":READY", target)
		if err != nil {
			return err
		}
		cleanup = false
		return nil
	})
}

func (p *Preparer) validateExistingWorktree(ctx context.Context, repo discovery.Repository, target, oid string) error {
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
	canonicalTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return err
	}
	listed, err := p.Git.Run(ctx, string(repo.MainPath), "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return err
	}
	for _, field := range strings.Split(listed.Stdout, "\x00") {
		if strings.HasPrefix(field, "worktree ") {
			registered, resolveErr := filepath.EvalSymlinks(strings.TrimPrefix(field, "worktree "))
			if resolveErr == nil && registered == canonicalTarget {
				return nil
			}
		}
	}
	return errors.New("worktree is not registered")
}

func (p *Preparer) copyIncludes(repo discovery.Repository, target string) error {
	patterns, err := discovery.ReadPatterns(filepath.Join(string(repo.MainPath), ".worktreeinclude"))
	if err != nil {
		return err
	}
	for _, pattern := range patterns {
		if filepath.IsAbs(pattern) || strings.HasPrefix(filepath.Clean(pattern), ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe .worktreeinclude pattern %q", pattern)
		}
		matches, err := filepath.Glob(filepath.Join(string(repo.MainPath), pattern))
		if err != nil {
			return err
		}
		for _, src := range matches {
			rel, _ := filepath.Rel(string(repo.MainPath), src)
			tracked, err := p.Git.Run(context.Background(), string(repo.MainPath), "ls-files", "--error-unmatch", "--", rel)
			if err == nil && strings.TrimSpace(tracked.Stdout) != "" {
				return fmt.Errorf(".worktreeinclude would overwrite tracked path %s", rel)
			}
			if err := copyPath(src, filepath.Join(target, rel)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *Preparer) createLinks(ctx context.Context, repo discovery.Repository, target string) error {
	patterns, err := discovery.ReadPatterns(filepath.Join(string(repo.MainPath), ".worktreelink"))
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
		dst := filepath.Join(target, clean)
		if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
			return err
		}
		source := filepath.Join(string(repo.MainPath), clean)
		if info, err := os.Lstat(dst); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				existing, readErr := os.Readlink(dst)
				if readErr == nil && existing == source {
					continue
				}
			}
			return fmt.Errorf(".worktreelink target collision %s", rel)
		}
		if err := os.Symlink(source, dst); err != nil {
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
	h := sha256.New()
	fmt.Fprintf(h, "schema=1\ngeneration=%d\noid=%s\n", generation, oid)
	for _, name := range []string{".worktreeinclude", ".worktreelink"} {
		data, err := os.ReadFile(filepath.Join(string(repo.MainPath), name))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		h.Write(data)
	}
	if o, ok := c.Repositories[string(repo.MainPath)]; ok {
		fmt.Fprint(h, o.Prepare.Command, o.Prepare.Version)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func MaterializeRoot(source, target string, rules config.Workspace) error {
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
		if err := copyPath(src, filepath.Join(target, clean)); err != nil {
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
		dst := filepath.Join(target, clean)
		if info, err := os.Lstat(dst); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				existing, readErr := os.Readlink(dst)
				if readErr == nil && existing == src {
					continue
				}
			}
			return fmt.Errorf("workspace root link collision %s", clean)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
			return err
		}
		if err := os.Symlink(src, dst); err != nil {
			return err
		}
	}
	return nil
}

func safeRelative(path string) (string, error) {
	clean := filepath.Clean(path)
	if path == "" || filepath.IsAbs(path) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe workspace root path %q", path)
	}
	return clean, nil
}

func copyPath(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("include symlinks are not followed")
	}
	if info.IsDir() {
		if err := os.MkdirAll(dst, 0700); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyPath(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}
	if existing, err := os.Lstat(dst); err == nil {
		if existing.Mode()&os.ModeSymlink != 0 || !existing.Mode().IsRegular() {
			return fmt.Errorf("copy target %s is not a regular file", dst)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}
