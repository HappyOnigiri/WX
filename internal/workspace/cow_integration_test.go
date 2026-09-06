package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
)

func cowGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	r := gitx.Runner{Timeout: 10 * time.Second}
	result, err := r.Run(context.Background(), directory, args...)
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, result.Stderr)
	}
	return strings.TrimSpace(result.Stdout)
}

func cowFixture(t *testing.T) (*Preparer, discovery.Repository, string, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	source := t.TempDir()
	cowGit(t, source, "init", "-b", "main")
	cowGit(t, source, "config", "user.name", "test")
	cowGit(t, source, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(source, "file"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cowGit(t, source, "add", ".")
	cowGit(t, source, "commit", "-m", "base")
	root := t.TempDir()
	slot := filepath.Join(root, testSlotRelPath)
	if err := os.MkdirAll(slot, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = root
	p := &Preparer{Git: &gitx.Runner{Timeout: 10 * time.Second}, Config: cfg, Ownership: allowOwnershipValidator{}, OwnedRoot: owner, RootPath: root, SlotPath: slot, RootID: testRootID, SlotRelPath: testSlotRelPath}
	repo := discovery.Repository{ID: testRepositoryID, MainPath: domain.CanonicalPath(source), CommonDir: domain.CanonicalPath(filepath.Join(source, ".git"))}
	return p, repo, cowGit(t, source, "rev-parse", "HEAD"), filepath.Join(slot, testRepositoryID)
}

func TestCOWPreparationModesAndHook(t *testing.T) {
	for _, mode := range []string{config.CopyModeAuto, config.CopyModeCOW, config.CopyModeCopy} {
		t.Run(mode, func(t *testing.T) {
			p, repo, oid, target := cowFixture(t)
			p.Config.Storage.CopyMode = mode
			p.Config.Repositories = map[string]config.Repository{string(repo.MainPath): {Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", "ls -i file > .before-inode"}}}}
			hook := filepath.Join(string(repo.MainPath), ".git", "hooks", "post-checkout")
			if err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf hook > .hook-ran\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			err := p.Prepare(context.Background(), repo, target, oid, testSlotID)
			if mode == config.CopyModeCOW && !cowAvailable() {
				if err == nil {
					t.Fatal("unsupported strict CoW succeeded")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(filepath.Join(target, ".hook-ran")); err != nil || string(data) != "hook" {
				t.Fatalf("checkout hook=%q %v", data, err)
			}

			captured, readErr := os.ReadFile(filepath.Join(target, ".before-inode"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			before, parseErr := strconv.ParseUint(strings.Fields(string(captured))[0], 10, 64)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			var after unix.Stat_t
			if err := unix.Stat(filepath.Join(target, "file"), &after); err != nil {
				t.Fatal(err)
			}
			wantReplacement := mode != config.CopyModeCopy && cowAvailable()
			if (before != after.Ino) != wantReplacement {
				t.Fatalf("mode=%s inode replacement=%t", mode, before != after.Ino)
			}
			if got := cowGit(t, target, "status", "--porcelain", "--untracked-files=no"); got != "" {
				t.Fatalf("dirty checkout: %s", got)
			}
			if got := cowGit(t, string(repo.MainPath), "rev-parse", "HEAD"); got != oid {
				t.Fatal("source HEAD changed")
			}
		})
	}
}

func TestCOWPrepareFallbackKeepsCheckout(t *testing.T) {
	for _, mode := range []string{config.CopyModeAuto, config.CopyModeCOW, config.CopyModeCopy} {
		t.Run(mode, func(t *testing.T) {
			// CoWのないplatformでは`cow`が donor 以前に落ちるため、symlink donor の分岐を検査できない。
			if mode == config.CopyModeCOW && !cowAvailable() {
				t.Skip("strict CoW is unsupported on this platform")
			}
			p, repo, oid, target := cowFixture(t)
			p.Config.Storage.CopyMode = mode
			source := filepath.Join(string(repo.MainPath), "file")
			if err := os.Remove(source); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("missing", source); err != nil {
				t.Fatal(err)
			}
			// symlink の donor は共有対象外というだけなので、`cow` でも準備は止めない。
			if err := p.Prepare(context.Background(), repo, target, oid, testSlotID); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(target, "file"))
			if err != nil || string(data) != "original\n" {
				t.Fatalf("fallback corrupted checkout: %q %v", data, err)
			}
		})
	}
}

func TestCOWRestorePreservesIndexAndDirtyBytes(t *testing.T) {
	p, repo, oid, target := cowFixture(t)
	if err := p.PrepareForRestore(context.Background(), repo, target, oid, testSlotID); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(target, "file")
	if err := os.WriteFile(file, []byte("staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cowGit(t, target, "add", "file")
	if err := os.WriteFile(file, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	beforeIndex := cowGit(t, target, "write-tree")
	beforeStatus := cowGit(t, target, "status", "--porcelain")
	identity, err := p.WorktreeIdentity(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.PrepareResumeWithIdentity(context.Background(), repo, target, oid, testSlotID, identity); err != nil {
		t.Fatal(err)
	}
	if after := cowGit(t, target, "write-tree"); after != beforeIndex {
		t.Fatal("staged index changed")
	}
	if after := cowGit(t, target, "status", "--porcelain"); after != beforeStatus {
		t.Fatalf("status changed %q -> %q", beforeStatus, after)
	}
}

func TestCOWReplayedTemporaryQuarantines(t *testing.T) {
	if !cowAvailable() {
		t.Skip("CoW platform required")
	}
	p, repo, oid, target := cowFixture(t)
	if err := p.PrepareForRestore(context.Background(), repo, target, oid, testSlotID); err != nil {
		t.Fatal(err)
	}
	temporary := filepath.Join(target, ".wx-cow-interrupted")
	if err := os.WriteFile(temporary, []byte("preserve me"), 0o644); err != nil {
		t.Fatal(err)
	}
	identity, err := p.WorktreeIdentity(target)
	if err != nil {
		t.Fatal(err)
	}
	p.Config.Storage.CopyMode = config.CopyModeCopy
	err = p.PrepareResumeWithIdentity(context.Background(), repo, target, oid, testSlotID, identity)
	if !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("replay error=%v", err)
	}
	if data, err := os.ReadFile(temporary); err != nil || string(data) != "preserve me" {
		t.Fatalf("temporary lost: %q %v", data, err)
	}
}

func TestCOWModeChangesFingerprint(t *testing.T) {
	p, repo, oid, _ := cowFixture(t)
	seen := map[string]bool{}
	for _, mode := range []string{config.CopyModeAuto, config.CopyModeCOW, config.CopyModeCopy} {
		p.Config.Storage.CopyMode = mode
		fp, err := Fingerprint(1, oid, repo, p.Config)
		if err != nil {
			t.Fatal(err)
		}
		if seen[fp] {
			t.Fatal("copy mode not included in fingerprint")
		}
		seen[fp] = true
	}
}

func TestCOWKeepsPathDependentFilterOutput(t *testing.T) {
	p, repo, _, target := cowFixture(t)
	source := string(repo.MainPath)
	cowGit(t, source, "config", "filter.location.clean", "sed 's|.*|TOKEN|'")
	cowGit(t, source, "config", "filter.location.smudge", "cat >/dev/null; pwd")
	if err := os.WriteFile(filepath.Join(source, ".gitattributes"), []byte("file filter=location\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cowGit(t, source, "add", ".")
	cowGit(t, source, "commit", "-m", "filter")
	oid := cowGit(t, source, "rev-parse", "HEAD")
	if err := p.Prepare(context.Background(), repo, target, oid, testSlotID); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(target, "file"))
	if err != nil || strings.TrimSpace(string(data)) != target {
		t.Fatalf("smudge output=%q %v", data, err)
	}
}

func TestCOWPreparationKeepsAmbiguousArtifacts(t *testing.T) {
	if !cowAvailable() {
		t.Skip("CoW platform required")
	}
	p, repo, oid, target := cowFixture(t)
	p.Config.Repositories = map[string]config.Repository{string(repo.MainPath): {Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", "printf retained > .wx-cow-ambiguous"}}}}
	err := p.Prepare(context.Background(), repo, target, oid, testSlotID)
	if !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("error=%v", err)
	}
	data, readErr := os.ReadFile(filepath.Join(target, ".wx-cow-ambiguous"))
	if readErr != nil || string(data) != "retained" {
		t.Fatalf("artifact lost: %q %v", data, readErr)
	}
}
