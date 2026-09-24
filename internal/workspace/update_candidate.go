package workspace

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
	"github.com/HappyOnigiri/WorktreeX/internal/state"
)

// ValidateUpdateCandidate は既知の更新不能条件をREADY slotの予約前に検査する。tree には配置計画と同じ要求OIDの列挙を渡す。
// 所有権とtracked cleanを確かめてから、worktreeを読む検査を並列に走らせる。
// `git status`はindexを書き換え得るため、worktreeを読む検査はその後に置く。
func (p *Preparer) ValidateUpdateCandidate(ctx context.Context, repo discovery.Repository, target, oldOID string, tree TreeLeaves, previous, desired []state.Placement) error {
	var diff updateTreeDiff
	// 差分はsourceのobjectだけを読みworktreeのindexに触れないため、準備検査と並べて走らせる。
	// 並び順が優先順位で、準備検査の失敗を差分の読み取り失敗より先に返す。
	if err := runChecksInOrder(ctx, []func(context.Context) error{
		func(ctx context.Context) error {
			return p.timePhase("candidate-ready", func() error { return p.ValidateReady(ctx, repo, target, oldOID) })
		},
		func(ctx context.Context) (err error) {
			diff, err = p.readUpdateTreeDiff(ctx, repo, oldOID, tree.OID)
			return err
		},
	}); err != nil {
		return err
	}
	if err := diff.rejectIneligible(); err != nil {
		return err
	}
	return p.timePhase("candidate-checks", func() error {
		root, err := p.destinationRoot(target)
		if err != nil {
			return err
		}
		defer func() { _ = root.Close() }()
		identity, err := p.WorktreeIdentity(target)
		if err != nil {
			return err
		}
		// 並び順がエラーの優先順位である。並列化前の直列の検査順に揃えている。
		return runChecksInOrder(ctx, []func(context.Context) error{
			func(ctx context.Context) error {
				return p.rejectUnrestorableWorktreeState(ctx, target, identity, diff, root, previous)
			},
			func(ctx context.Context) error {
				return p.rejectUpdateCollisions(ctx, target, identity, root, diff, tree, previous, desired)
			},
		})
	})
}

// TreeLeaves は1つのOIDのtreeにある葉（blob・symlink・gitlink）のpath集合である。
// 配置計画と更新候補の衝突検査が同じ列挙を共有し、大きなtreeを貸出のたびに何度も読まないために持ち回る。
type TreeLeaves struct {
	OID   string
	paths map[string]bool
}

// ListTreeLeaves は oid のtreeをsource repositoryで1回だけ列挙する。
func (p *Preparer) ListTreeLeaves(ctx context.Context, repo discovery.Repository, oid string) (TreeLeaves, error) {
	paths, err := p.trackedPathsAt(ctx, repo, oid)
	if err != nil {
		return TreeLeaves{}, err
	}
	return TreeLeaves{OID: oid, paths: paths}, nil
}

// conflicts は path が葉と同じか、葉の祖先か子孫であるかを返す。
// directories は葉の祖先directoryの集合で、呼び出し側が1回だけ組んで使い回す。
func (t TreeLeaves) conflicts(path string, directories map[string]bool) bool {
	if t.paths[path] || directories[path] {
		return true
	}
	for parent := filepath.Dir(path); parent != "." && parent != string(filepath.Separator); parent = filepath.Dir(parent) {
		if t.paths[parent] {
			return true
		}
	}
	return false
}

func (t TreeLeaves) directories() map[string]bool {
	out := map[string]bool{}
	for path := range t.paths {
		for parent := filepath.Dir(path); parent != "." && parent != string(filepath.Separator) && !out[parent]; parent = filepath.Dir(parent) {
			out[parent] = true
		}
	}
	return out
}

// updateTreeDiff は旧OIDから要求OIDへの差分を1回のdiff-treeで読んだ結果で、予約前の検査が共有する。
type updateTreeDiff struct {
	// newModes は差分に乗る全pathの要求OIDでのmodeである。削除されたpathは000000になる。
	newModes map[string]string
	// added は旧treeに葉として無く要求OIDで現れるpathで、fileとdirectoryの型変化で現れる葉も含む。
	added      []string
	modules    bool
	gitlinks   bool
	attributes bool
}

// readUpdateTreeDiff は source で旧OIDと要求OIDのtreeを比べる。
// rename検出は旧名を落として集合を狭めるため切り、submoduleの無視設定はgitlinkの変化を隠すため打ち消す。
func (p *Preparer) readUpdateTreeDiff(ctx context.Context, repo discovery.Repository, oldOID, newOID string) (updateTreeDiff, error) {
	result, err := p.Git.Run(ctx, string(repo.MainPath), "diff-tree", "-r", "-z", "--raw", "--no-renames", "--ignore-submodules=none", oldOID, newOID)
	if err != nil {
		return updateTreeDiff{}, err
	}
	return parseUpdateTreeDiff(result.Stdout)
}

// parseUpdateTreeDiff は`diff-tree -r -z --raw`の出力を読む。
// 1件は「:旧mode 新mode 旧OID 新OID 状態」とpathのNUL区切りの組で、rename検出を切るとpathは1つだけになる。
func parseUpdateTreeDiff(output string) (updateTreeDiff, error) {
	diff := updateTreeDiff{newModes: map[string]string{}}
	fields := strings.Split(output, "\x00")
	for i := 0; i < len(fields); i++ {
		if fields[i] == "" {
			continue
		}
		meta := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		if !strings.HasPrefix(fields[i], ":") || len(meta) != 5 || i+1 >= len(fields) || fields[i+1] == "" {
			return updateTreeDiff{}, fmt.Errorf("unexpected diff-tree record %q", fields[i])
		}
		i++
		path, oldMode, newMode, status := fields[i], meta[0], meta[1], meta[4]
		diff.newModes[path] = newMode
		if status == "A" {
			diff.added = append(diff.added, filepath.Clean(filepath.FromSlash(path)))
		}
		if oldMode == "160000" || newMode == "160000" {
			diff.gitlinks = true
		}
		if path == ".gitmodules" || strings.HasPrefix(path, ".gitmodules/") {
			diff.modules = true
		}
		if slices.Contains(strings.Split(path, "/"), ".gitattributes") {
			diff.attributes = true
		}
	}
	return diff, nil
}

// rejectIneligible は差分だけで決まる更新不能条件を返す。
// 更新は`checkout --detach --force`だけでsubmoduleを再同期しないため、.gitmodulesとgitlinkの変化を弾く。
// .gitattributesの変化も弾く。再展開は`git checkout-index`では済まず、内容が同じでstat cacheの一致するfileだけが旧属性のまま残る。
// commentlint:allow-long -- 3つの条件それぞれの理由を1か所で読めるようにする
func (d updateTreeDiff) rejectIneligible() error {
	switch {
	case d.modules:
		return fmt.Errorf("%w: submodule configuration changed", ErrUpdateIneligible)
	case d.gitlinks:
		return fmt.Errorf("%w: submodule configuration or gitlink OID changed", ErrUpdateIneligible)
	case d.attributes:
		return fmt.Errorf("%w: .gitattributes changed between the standby and the requested OID", ErrUpdateIneligible)
	}
	return nil
}

// rejectUpdateCollisions は、更新で現れるpathがworktreeのuntracked・ignoredの実体や、要求OIDのtrackedと重ならないかを検査する。
// checkoutは`--force`でuntrackedのfileを黙って上書きし、予約後に再検査しないので、この検査が唯一の防御である。
// HEADが旧OIDでtracked cleanなら、untrackedの実体と重なり得る新しいtrackedは旧treeに葉として無いpathに限られる。
// 配置は旧OIDとの差分と関係なく重なり得るため、旧配置に無いpathを全て調べる。
// 旧配置と同じpathのuntrackedは、書込み前に取り除く対象なので衝突に数えない。
// commentlint:allow-long -- 差分へ限定してよい根拠と、配置を差分に限らない理由を並べて残す
func (p *Preparer) rejectUpdateCollisions(ctx context.Context, target, identity string, root *os.Root, diff updateTreeDiff, tree TreeLeaves, previous, desired []state.Placement) error {
	old := placementPathSet(previous)
	desiredPaths := placementPathSet(desired)
	targets := slices.Clone(diff.added)
	for path := range desiredPaths {
		if !old[path] {
			targets = append(targets, path)
		}
	}
	probes, err := updateCollisionProbes(root, targets)
	if err != nil {
		return err
	}
	untracked, err := p.untrackedPathsUnder(ctx, target, identity, probes)
	if err != nil {
		return err
	}
	for _, path := range untracked {
		if !old[path] {
			return fmt.Errorf("%w: untracked or ignored path %s conflicts with standby update", ErrUpdateIneligible, path)
		}
	}
	if len(desiredPaths) == 0 {
		return nil
	}
	directories := tree.directories()
	for _, path := range slices.Sorted(maps.Keys(desiredPaths)) {
		if tree.conflicts(path, directories) {
			return fmt.Errorf("%w: placement path %s becomes tracked at requested OID", ErrUpdateIneligible, path)
		}
	}
	return nil
}

// updateCollisionProbes は、対象pathと重なり得る実体を持つpathをpinしたroot上のlstatで集める。
// 祖先がfile・symlink・nested repositoryなら祖先を、対象自体が存在すれば対象を返し、どちらも無い対象は何も返さない。
// 返したpathの配下にuntracked・ignoredの葉があるかはGitに判定させる。
// 旧treeの葉（fileとdirectoryの型変化、大文字小文字だけのrename）はindexに載っているため、Gitの列挙には挙がらない。
// 空directoryと旧trackedの葉だけを含むdirectoryも同じ理由で挙がらず、衝突に数えない。
// commentlint:allow-long -- lstatで絞る範囲と、残りをGitに判定させる理由を保守時に確認できるようにする
func updateCollisionProbes(root *os.Root, targets []string) ([]string, error) {
	repositories := map[string]bool{}
	checked := map[string]bool{}
	probes := map[string]bool{}
	for _, target := range targets {
		components := strings.Split(filepath.Clean(target), string(filepath.Separator))
		current := ""
		for index, component := range components {
			current = filepath.Join(current, component)
			info, err := root.Lstat(current)
			if errors.Is(err, os.ErrNotExist) {
				break
			}
			if err != nil {
				return nil, err
			}
			if index == len(components)-1 || !info.IsDir() {
				probes[current] = true
				break
			}
			if !checked[current] {
				checked[current] = true
				_, markerErr := root.Lstat(filepath.Join(current, ".git"))
				if markerErr != nil && !errors.Is(markerErr, os.ErrNotExist) {
					return nil, markerErr
				}
				repositories[current] = markerErr == nil
			}
			if repositories[current] {
				probes[current] = true
				break
			}
		}
	}
	return slices.Sorted(maps.Keys(probes)), nil
}

// untrackedProbeBatch は1回のls-filesへ渡すpathspecの上限で、引数長の制限に掛からないようにする。
const untrackedProbeBatch = 256

// untrackedPathsUnder は probes 自身とその配下にあるuntrackedとignoredのpathを列挙する。
// 除外指定を付けないls-filesは、untrackedとignoredを別々に列挙した和集合を1回で返す。nested repositoryは1件のdirectoryになる。
// pathspecには`:(literal)`を付ける。pathにglob文字が含まれるとmagicとして解釈されるためである。
// commentlint:allow-long -- 列挙の意味と pathspec の注意を doc comment にまとめる
func (p *Preparer) untrackedPathsUnder(ctx context.Context, target, identity string, probes []string) ([]string, error) {
	var out []string
	for start := 0; start < len(probes); start += untrackedProbeBatch {
		args := []string{"ls-files", "--others", "-z", "--"}
		for _, probe := range probes[start:min(len(probes), start+untrackedProbeBatch)] {
			args = append(args, ":(literal)"+filepath.ToSlash(probe))
		}
		result, err := p.RunGitInWorktree(ctx, target, identity, nil, nil, args...)
		if err != nil {
			return nil, err
		}
		for _, entry := range strings.Split(result.Stdout, "\x00") {
			if entry != "" {
				out = append(out, filepath.Clean(filepath.FromSlash(entry)))
			}
		}
	}
	slices.Sort(out)
	return out, nil
}

// runChecksInOrder は全ての検査を並列に実行し、完了を待ってからslice順で最初のエラーを返す。
// 呼び出し側はエラーの種類でSTALE化するかを分けるため、到着順に採ると同じ状態でも貸出ごとに判定が変わる。
// 失敗した検査より後ろの順位だけを取り消す。前の順位を取り消すと、取消しのエラーが本来のエラーを覆い得る。
func runChecksInOrder(ctx context.Context, checks []func(context.Context) error) error {
	errs := make([]error, len(checks))
	contexts := make([]context.Context, len(checks))
	cancels := make([]context.CancelFunc, len(checks))
	for i := range checks {
		contexts[i], cancels[i] = context.WithCancel(ctx)
	}
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()
	var wait sync.WaitGroup
	for i, check := range checks {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if errs[i] = check(contexts[i]); errs[i] != nil {
				for _, cancel := range cancels[i+1:] {
					cancel()
				}
			}
		}()
	}
	wait.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// rejectUnrestorableWorktreeState はstandbyの実体に依存する更新不能条件を検査する。
// flag付きpathの退避元と記録済み配置は同じrootで読むため、1つの検査にまとめている。
func (p *Preparer) rejectUnrestorableWorktreeState(ctx context.Context, target, identity string, diff updateTreeDiff, root *os.Root, previous []state.Placement) error {
	if err := p.rejectUnrestorableFlaggedPaths(ctx, target, identity, diff, root); err != nil {
		return err
	}
	if err := validateRecordedPlacements(root, previous); err != nil {
		return fmt.Errorf("%w: %w", ErrUpdateIneligible, err)
	}
	return nil
}
