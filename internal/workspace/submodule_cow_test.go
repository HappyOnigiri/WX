package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/state"
)

// 実体化した子の checkout も、親と同じ下限・OID skip・置換方式で共有する。
func TestPrepareSharesSubmoduleCheckout(t *testing.T) {
	t.Parallel()
	f := newSubmoduleFixture(t)
	advanceSubmoduleFixture(t, f)

	f.preparer.Config.Storage.CopyMode = config.CopyModeAuto
	f.preparer.Config.Storage.COWMinSizeKiB = 0
	f.preparer.Phases = &PhaseTimings{}
	f.preparer.Config.Repositories = map[string]config.Repository{
		string(f.repo.MainPath): {Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", "ls -i sub/kid/kid.txt > .before-submodule-inode"}}},
	}
	if err := f.preparer.Prepare(context.Background(), f.repo, f.target, f.head, "slot"); err != nil {
		t.Fatal(err)
	}
	if !cowAvailable() {
		identity, err := f.preparer.WorktreeIdentity(f.target)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.preparer.compactOwnedSubmoduleWorktree(context.Background(), f.repo, f.target, f.head, "slot", preparePhaseCreate, identity, false); !errors.Is(err, unix.ENOTSUP) {
			t.Fatalf("unsupported submodule CoW error=%v", err)
		}
		return
	}
	before, err := os.ReadFile(filepath.Join(f.target, ".before-submodule-inode"))
	if err != nil {
		t.Fatal(err)
	}
	beforeInode, err := strconv.ParseUint(strings.Fields(string(before))[0], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	var after unix.Stat_t
	if err := unix.Stat(filepath.Join(f.target, submodulePath, "kid.txt"), &after); err != nil {
		t.Fatal(err)
	}
	if beforeInode == after.Ino {
		t.Fatalf("submodule checkout was not replaced: inode=%d", after.Ino)
	}
	if data, err := os.ReadFile(filepath.Join(f.target, submodulePath, "kid.txt")); err != nil || string(data) != cowBody {
		t.Fatalf("submodule bytes=%d err=%v", len(data), err)
	}
	seen := false
	for _, phase := range f.preparer.Phases.Phases() {
		if phase.Name == "submodule-cow" {
			seen = true
		}
	}
	if !seen {
		t.Fatal("submodule-cow phase missing")
	}
}

// 二段階準備でも、親の先行配置が完了して submodule が後段で実体化する順序を通す。
func TestPrepareStagedSharesSubmoduleCheckout(t *testing.T) {
	t.Parallel()
	f := newSubmoduleFixture(t)
	advanceSubmoduleFixture(t, f)
	f.preparer.Config.Storage.CopyMode = config.CopyModeAuto
	f.preparer.Config.Storage.COWMinSizeKiB = 0
	f.preparer.Phases = &PhaseTimings{}
	var beforeInode uint64
	f.runner.SetBeforeRunAtHook(func(args []string) {
		if beforeInode != 0 || !strings.Contains(strings.Join(args, " "), "--recurse-submodules") || strings.Contains(strings.Join(args, " "), "--no-optional-locks") {
			return
		}
		var info unix.Stat_t
		if err := unix.Stat(filepath.Join(f.target, submodulePath, "kid.txt"), &info); err == nil {
			beforeInode = info.Ino
		}
	})
	if _, err := f.preparer.PrepareStaged(context.Background(), "slot", []Preparation{{Repository: f.repo, Target: f.target, OID: f.head}}, nil, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !cowAvailable() {
		identity, err := f.preparer.WorktreeIdentity(f.target)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.preparer.compactOwnedSubmoduleWorktree(context.Background(), f.repo, f.target, f.head, "slot", preparePhaseCreate, identity, false); !errors.Is(err, unix.ENOTSUP) {
			t.Fatalf("unsupported staged submodule CoW error=%v", err)
		}
		return
	}
	var after unix.Stat_t
	if err := unix.Stat(filepath.Join(f.target, submodulePath, "kid.txt"), &after); err != nil {
		t.Fatal(err)
	}
	if beforeInode == 0 || beforeInode == after.Ino {
		t.Fatalf("staged submodule checkout was not replaced: before=%d after=%d", beforeInode, after.Ino)
	}
}

func advanceSubmoduleFixture(t *testing.T, f *submoduleFixture) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.child, "kid.txt"), []byte(cowBody), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, f.child, "add", "kid.txt")
	gitCommand(t, f.child, "commit", "-m", "child large")
	childOID := gitOutput(t, f.child, "rev-parse", "HEAD")
	gitCommand(t, f.moduleDir(), "fetch", "origin", childOID)
	gitCommand(t, f.repository, "update-index", "--cacheinfo", "160000,"+childOID+","+submodulePath)
	gitCommand(t, f.repository, "commit", "-m", "advance gitlink")
	f.head = gitOutput(t, f.repository, "rev-parse", "HEAD")
	gitCommand(t, f.repository, "-c", "protocol.file.allow=always", "submodule", "update", "--init", submodulePath)
}

// copy 指定では、子の index 収集を含めて submodule-CoW の Git を起動しない。
func TestPrepareCopyModeSkipsSubmoduleCOWGit(t *testing.T) {
	t.Parallel()
	f := newSubmoduleFixture(t)
	f.preparer.Config.Storage.CopyMode = config.CopyModeCopy
	var commands []string
	f.runner.SetBeforeRunAtHook(func(args []string) { commands = append(commands, strings.Join(args, " ")) })
	if err := f.preparer.Prepare(context.Background(), f.repo, f.target, f.head, "slot"); err != nil {
		t.Fatal(err)
	}
	for _, command := range commands {
		if strings.Contains(command, "--recurse-submodules") || (strings.Contains(command, "submodule.active=") && !strings.Contains(command, " hook run ")) {
			t.Fatalf("submodule-CoW command ran in copy mode: %q", command)
		}
	}
}

// 復元・再利用では子の交換途中に残った予約名を所有権不明として止める。
func TestPrepareResumeRejectsSubmoduleCOWTemporary(t *testing.T) {
	t.Parallel()
	f := newSubmoduleFixture(t)
	if err := f.preparer.PrepareForRestore(context.Background(), f.repo, f.target, f.head, "slot"); err != nil {
		t.Fatal(err)
	}
	temporary := filepath.Join(f.target, submodulePath, ".wx-cow-interrupted")
	if err := os.WriteFile(temporary, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := f.preparer.WorktreeIdentity(f.target)
	if err != nil {
		t.Fatal(err)
	}
	if !cowAvailable() {
		err = f.preparer.compactOwnedSubmoduleWorktree(context.Background(), f.repo, f.target, f.head, "slot", preparePhaseRestore, identity, true)
		if !errors.Is(err, state.ErrOwnership) {
			t.Fatalf("unsupported resume error=%v, want ownership failure", err)
		}
		return
	}
	err = f.preparer.PrepareResumeWithIdentity(context.Background(), f.repo, f.target, f.head, "slot", identity)
	if !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("resume error=%v, want ownership failure", err)
	}
	if data, readErr := os.ReadFile(temporary); readErr != nil || string(data) != "keep" {
		t.Fatalf("temporary=%q err=%v", data, readErr)
	}
}

// 復元した子の staged/unstaged bytes は、main 側 index と一致しないため共有で上書きしない。
func TestPrepareResumePreservesDirtySubmoduleBytes(t *testing.T) {
	t.Parallel()
	f := newSubmoduleFixture(t)
	advanceSubmoduleFixture(t, f)
	if err := f.preparer.PrepareForRestore(context.Background(), f.repo, f.target, f.head, "slot"); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(f.target, submodulePath, "kid.txt")
	if err := os.WriteFile(file, []byte("dirty child\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := f.preparer.WorktreeIdentity(f.target)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.preparer.PrepareResumeWithIdentity(context.Background(), f.repo, f.target, f.head, "slot", identity); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "dirty child\n" {
		t.Fatalf("restored child bytes=%q err=%v", data, err)
	}
}
