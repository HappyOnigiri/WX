package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WorktreeX/internal/state"
)

func mutationBoundaryHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// UpdateLocked は既存配置を検証してから更新を始める。検証を反転すると、古い配置を
// 差し替えて最後の検証だけを通せるため、更新全体を失敗させる契約を直接固定する。
// testlint:allow-serial -- cowFixture がプロセス環境を変更する。
func TestUpdateLockedPropagatesRecordedPlacementValidationError(t *testing.T) {
	ctx := context.Background()
	p, repo, oid, target := cowFixture(t)
	if err := p.Prepare(ctx, repo, target, oid, testSlotID); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(string(repo.MainPath), "file")
	desired := state.Placement{
		RelativePath:  "file",
		Kind:          "copy",
		SourcePath:    source,
		ContentSHA256: mutationBoundaryHash([]byte(cowBody)),
	}
	previous := desired
	previous.ContentSHA256 = strings.Repeat("0", 64)
	_, err := p.UpdateLocked(ctx, repo, target, oid, oid, testSlotID, []state.Placement{previous}, []state.Placement{desired})
	if err == nil || !strings.Contains(err.Error(), "recorded copy file changed content") {
		t.Fatalf("invalid recorded placement error=%v, want validation failure", err)
	}
}

// 更新先の link 判定が実行障害になったときは、そのエラーを無視せず返す。
// testlint:allow-serial -- cowFixture と PATH の差し替えを組み合わせる。
func TestUpdateLockedPropagatesUpdateLinkFilterError(t *testing.T) {
	ctx := context.Background()
	p, repo, oid, target := cowFixture(t)
	if err := p.Prepare(ctx, repo, target, oid, testSlotID); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(string(repo.MainPath), "link-source")
	if err := os.WriteFile(source, []byte("source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(source, filepath.Join(target, "linked")); err != nil {
		t.Fatal(err)
	}
	placement := state.Placement{RelativePath: "linked", Kind: "link", SourcePath: source}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	wrapper := filepath.Join(bin, "git")
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"check-ignore\" ]; then\n  printf 'forced check-ignore failure\\n' >&2\n  exit 2\nfi\nexec %q \"$@\"\n", gitPath)
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err = p.UpdateLocked(ctx, repo, target, oid, oid, testSlotID, []state.Placement{placement}, []state.Placement{placement})
	if err == nil || !strings.Contains(err.Error(), "check-ignore") {
		t.Fatalf("link filter error=%v, want check-ignore failure", err)
	}
}

// filter が link を除外した後の配置削除に失敗した場合も、更新を続行してはならない。
// testlint:allow-serial -- cowFixture がプロセス環境を変更する。
func TestUpdateLockedPropagatesUpdatePlacementRemovalError(t *testing.T) {
	ctx := context.Background()
	p, repo, oid, target := cowFixture(t)
	if err := p.Prepare(ctx, repo, target, oid, testSlotID); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(string(repo.MainPath), "link-source")
	if err := os.WriteFile(source, []byte("source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(target, "blocked")
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(blocked, "linked")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })
	placement := state.Placement{RelativePath: "blocked/linked", Kind: "link", SourcePath: source}
	p.Git.SetBeforeRunAtHook(func(args []string) {
		if strings.Join(args, "\x00") != "check-ignore\x00-q\x00--\x00blocked/linked" {
			return
		}
		if err := os.Chmod(blocked, 0); err != nil {
			t.Fatal(err)
		}
	})
	_, err := p.UpdateLocked(ctx, repo, target, oid, oid, testSlotID, []state.Placement{placement}, []state.Placement{placement})
	if err == nil || !strings.Contains(err.Error(), "remove obsolete placement blocked/linked") {
		t.Fatalf("placement removal error=%v, want remove failure", err)
	}
}

// 更新後の COW 候補計算に失敗した場合は、nil scope のまま全体共有へ倒れずに失敗を返す。
// testlint:allow-serial -- cowFixture と PATH の差し替えを組み合わせる。
func TestUpdateLockedPropagatesCOWScopeError(t *testing.T) {
	ctx := context.Background()
	p, repo, oid, target := cowFixture(t)
	if err := p.Prepare(ctx, repo, target, oid, testSlotID); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "fail-diff.marker")
	failed := filepath.Join(t.TempDir(), "fail-diff.once")
	t.Setenv("WX_TEST_DIFF_MARKER", marker)
	t.Setenv("WX_TEST_DIFF_FAILED", failed)
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	wrapper := filepath.Join(bin, "git")
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"diff\" ] && [ \"$2\" = \"--name-only\" ] && [ \"$3\" = \"--no-renames\" ] && [ -e \"$WX_TEST_DIFF_MARKER\" ] && [ ! -e \"$WX_TEST_DIFF_FAILED\" ]; then\n  : > \"$WX_TEST_DIFF_FAILED\"\n  printf 'forced update scope failure\\n' >&2\n  exit 2\nfi\nexec %q \"$@\"\n", gitPath)
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	// checkout より後の最初の diff は候補計算だけなので、checkout を見た時点で失敗を仕込む。
	// 検証の回数は copy mode や CoW の有無で変わるため、それを数えて時点を決めない。
	p.Git.SetBeforeRunAtHook(func(args []string) {
		if !strings.Contains(strings.Join(args, "\x00"), "\x00checkout\x00") {
			return
		}
		if err := os.WriteFile(marker, []byte("ready\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	_, err = p.UpdateLocked(ctx, repo, target, oid, oid, testSlotID, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "git diff failed with exit 2") {
		t.Fatalf("update COW scope error=%v, want diff failure", err)
	}
	if _, statErr := os.Stat(failed); statErr != nil {
		t.Fatalf("scope fault was not triggered: %v", statErr)
	}
}
