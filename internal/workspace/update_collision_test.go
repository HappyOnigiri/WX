package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
	"github.com/HappyOnigiri/WorktreeX/internal/state"
)

// collisionPlacementBody は旧配置として記録する copy の内容である。
const collisionPlacementBody = "placed\n"

// validateCollisionCandidate は要求OIDのtreeを列挙してから更新候補を検査する。
func validateCollisionCandidate(ctx context.Context, p *Preparer, repo discovery.Repository, target, oldOID, newOID string, previous, desired []state.Placement) error {
	tree, err := p.ListTreeLeaves(ctx, repo, newOID)
	if err != nil {
		return err
	}
	return p.ValidateUpdateCandidate(ctx, repo, target, oldOID, tree, previous, desired)
}

// 予約前の衝突検査の判定を、untracked・ignoredの実体の種類と、新しいtrackedや配置との関係（同じpath・祖先・子孫）の組ごとに固定する。
// 旧treeの葉だったpathの型変化と大文字小文字だけのrenameは、untrackedの実体が無いので拒否しない。
// testlint:allow-serial -- cowFixture が隔離 repository の構築中に HOME を変更する。
func TestValidateUpdateCandidateCollisions(t *testing.T) {
	type testCase struct {
		name string
		// change は source の base に加える変更で、結果を要求OIDとして commit する。nil なら base のまま更新する。
		change func(t *testing.T, main string)
		// standby は準備済み standby の worktree に作る untracked・ignored の実体である。
		standby func(t *testing.T, target string)
		// previous は旧配置、desired は要求OIDでの配置の path である。旧配置は内容つきの copy として記録する。
		previous []string
		desired  []string
		reject   string
	}
	addTracked := func(paths ...string) func(*testing.T, string) {
		return func(t *testing.T, main string) {
			for _, path := range paths {
				writeTestFile(t, filepath.Join(main, path), "tracked\n")
			}
		}
	}
	untracked := func(paths ...string) func(*testing.T, string) {
		return func(t *testing.T, target string) {
			for _, path := range paths {
				writeTestFile(t, filepath.Join(target, path), "local\n")
			}
		}
	}
	nested := func(t *testing.T, target string) {
		writeTestFile(t, filepath.Join(target, "nested", "inner"), "inner\n")
		cowGit(t, filepath.Join(target, "nested"), "init", "-q")
	}
	emptyDirectory := func(t *testing.T, target string) {
		if err := os.MkdirAll(filepath.Join(target, "empty"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	const (
		untrackedConflict = "untracked or ignored path"
		trackedPlacement  = "becomes tracked"
	)
	for _, tc := range []testCase{
		{name: "untracked file at new tracked path", change: addTracked("n"), standby: untracked("n"), reject: untrackedConflict},
		{name: "untracked file above new tracked path", change: addTracked("n/x"), standby: untracked("n"), reject: untrackedConflict},
		{name: "untracked file below new tracked path", change: addTracked("n"), standby: untracked("n/x"), reject: untrackedConflict},
		{name: "ignored file at new tracked path", change: addTracked("n.ign"), standby: untracked("n.ign"), reject: untrackedConflict},
		{name: "ignored directory at new tracked path", change: addTracked("ignored-dir"), standby: untracked("ignored-dir/f"), reject: untrackedConflict},
		{name: "ignored file above new tracked path", change: addTracked("ignored-dir/f/g"), standby: untracked("ignored-dir/f"), reject: untrackedConflict},
		{name: "nested repository above new tracked path", change: addTracked("nested/x"), standby: nested, reject: untrackedConflict},
		{name: "nested repository at new tracked path", change: addTracked("nested"), standby: nested, reject: untrackedConflict},
		{name: "empty directory above new tracked path", change: addTracked("empty/x"), standby: emptyDirectory},
		{name: "empty directory at new tracked path", change: addTracked("empty"), standby: emptyDirectory},
		{name: "unrelated untracked file", change: addTracked("n"), standby: untracked("other", "n2", "nx/y")},
		{name: "old file becomes a directory", change: func(t *testing.T, main string) {
			cowGit(t, main, "rm", "-q", "typechange")
			addTracked("typechange/x")(t, main)
		}},
		{name: "old directory becomes a file", change: func(t *testing.T, main string) {
			cowGit(t, main, "rm", "-rq", "tracked-dir")
			addTracked("tracked-dir")(t, main)
		}},
		{name: "case-only rename", change: func(t *testing.T, main string) {
			cowGit(t, main, "mv", "Case", "case")
		}},
		{name: "old placement becomes tracked", change: addTracked("p.local"), previous: []string{"p.local"}},
		{name: "untracked file at new placement", standby: untracked("d"), desired: []string{"d"}, reject: untrackedConflict},
		{name: "untracked file above new placement", standby: untracked("d"), desired: []string{"d/x"}, reject: untrackedConflict},
		{name: "untracked file below new placement", standby: untracked("d/x"), desired: []string{"d"}, reject: untrackedConflict},
		{name: "ignored directory at new placement", standby: untracked("ignored-dir/f"), desired: []string{"ignored-dir"}, reject: untrackedConflict},
		{name: "nested repository above new placement", standby: nested, desired: []string{"nested/cfg"}, reject: untrackedConflict},
		{name: "empty directory above new placement", standby: emptyDirectory, desired: []string{"empty/cfg"}},
		{name: "kept placement", previous: []string{"p.local"}, desired: []string{"p.local"}},
		{name: "new placement beside old placement", previous: []string{"cfg/a"}, desired: []string{"cfg/a", "cfg/b"}},
		{name: "new placement over old placement directory", previous: []string{"cfg/a"}, desired: []string{"cfg"}},
		{name: "placement at new tracked path parent", change: addTracked("t/x"), desired: []string{"t"}, reject: trackedPlacement},
		{name: "placement below new tracked path", change: addTracked("t/x"), desired: []string{"t/x/y"}, reject: trackedPlacement},
		{name: "placement below old tracked file", desired: []string{"file/cfg"}, reject: trackedPlacement},
		{name: "placement above old tracked file", desired: []string{"tracked-dir"}, reject: trackedPlacement},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			p, repo, _, target := cowFixture(t)
			main := string(repo.MainPath)
			writeTestFile(t, filepath.Join(main, ".gitignore"), "*.ign\nignored-dir/\n")
			addTracked("typechange", "tracked-dir/leaf", "Case")(t, main)
			cowGit(t, main, "add", "-A", "-f")
			cowGit(t, main, "commit", "-q", "-m", "collision base")
			baseOID := cowGit(t, main, "rev-parse", "HEAD")
			if err := p.Prepare(ctx, repo, target, baseOID, testSlotID); err != nil {
				t.Fatal(err)
			}
			newOID := baseOID
			if tc.change != nil {
				tc.change(t, main)
				// ignoreの対象を tracked にする変更も作るため、ignore 規則を無視して加える。
				cowGit(t, main, "add", "-A", "-f")
				cowGit(t, main, "commit", "-q", "-m", "collision change")
				newOID = cowGit(t, main, "rev-parse", "HEAD")
			}
			if tc.standby != nil {
				tc.standby(t, target)
			}
			sum := sha256.Sum256([]byte(collisionPlacementBody))
			var previous, desired []state.Placement
			for _, path := range tc.previous {
				writeTestFile(t, filepath.Join(target, path), collisionPlacementBody)
				previous = append(previous, state.Placement{RelativePath: path, Kind: "copy", ContentSHA256: hex.EncodeToString(sum[:])})
			}
			for _, path := range tc.desired {
				desired = append(desired, state.Placement{RelativePath: path, Kind: "copy"})
			}
			err := validateCollisionCandidate(ctx, p, repo, target, baseOID, newOID, previous, desired)
			if tc.reject == "" {
				if err != nil {
					t.Fatalf("eligible update was rejected: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrUpdateIneligible) || !strings.Contains(err.Error(), tc.reject) {
				t.Fatalf("error=%v, want ErrUpdateIneligible containing %q", err, tc.reject)
			}
		})
	}
}
