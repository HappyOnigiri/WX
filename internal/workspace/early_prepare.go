package workspace

import (
	"context"
	"fmt"
	"runtime"
	"strconv"
	"strings"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

// Preparation は一つの repository に配置する確定済み OID と slot 内の場所である。
type Preparation struct {
	Repository  discovery.Repository
	Target, OID string
}

type stagedRepository struct {
	Preparation
	locked *lockedTarget
	plan   earlyPlan
}

// PrepareStaged は全 repository の先行配置を一巡してから残りを展開する。
// 呼び出し元は slot lock を保持し、開始を永続化しておく。失敗時も部分展開を削除せず残す。rootStage は非 Git workspace root の配置、earlyReady は全先行配置の永続化を受け持つ。
// 戻り値は repository ID ごとの、この呼び出しで実際に配置した include/link である。呼び出し元は規則を読み直さずこれを配置履歴にする。読み直すと、準備中の規則変更で記録と実体が食い違う。
func (p *Preparer) PrepareStaged(ctx context.Context, slotID string, repositories []Preparation, rootStage func(bool) error, earlyReady func() error) (map[string][]state.Placement, error) {
	var prepared []*stagedRepository
	defer func() {
		for _, repo := range prepared {
			repo.locked.close()
		}
	}()
	for index, request := range repositories {
		// 区間名は repository ごとに同じものが繰り返されるため、進捗の表示が何周目かを読めるよう対象を添える。
		p.Phases.Scope(RepositoryScope(request.Repository, index+1, len(repositories)))
		root, target, err := p.prepareTarget(request.Target)
		if err != nil {
			return nil, err
		}
		item := &stagedRepository{Preparation: request, plan: earlyPlan{log: p.Log}}
		err = p.timePhase("git-register", func() error {
			return p.Git.WithCommonDirLock(ctx, string(request.Repository.CommonDir), func(lockCtx context.Context) error {
				var beginErr error
				item.locked, beginErr = p.beginPrepare(lockCtx, request.Repository, target, request.OID, slotID, preparePhaseCreate, root)
				return beginErr
			})
		})
		if err != nil {
			return nil, err
		}
		prepared = append(prepared, item)
		if item.locked.existing {
			return nil, fmt.Errorf("%w: staged preparation target already exists", state.ErrOwnership)
		}
		if err := p.timePhase("early-index", func() error { return p.buildEarlyPlan(ctx, item) }); err != nil {
			return nil, err
		}
		if err := p.timePhase("early-checkout", func() error { return p.checkoutStage(ctx, item, true, nil) }); err != nil {
			return nil, err
		}
		if err := p.timePhase("early-place", func() error {
			return p.materializePlan(ctx, item.Repository, item.locked, &item.plan, true)
		}); err != nil {
			return nil, err
		}
	}
	// workspace root と全 repository をまとめて見る区間は、特定の repository には属さない。
	p.Phases.Scope(PhaseScope{})
	if rootStage != nil {
		if err := p.timePhase("early-root", func() error { return rootStage(true) }); err != nil {
			return nil, err
		}
	}
	if err := p.timePhase("early-ready", func() error {
		for _, item := range prepared {
			if err := p.validatePreparedTarget(ctx, item.Repository, item.Target, item.OID, slotID, preparePhaseCreate, item.locked.root, item.locked.relative, item.locked.identity, "validate early readiness"); err != nil {
				return err
			}
		}
		return earlyReady()
	}); err != nil {
		return nil, err
	}
	for index, item := range prepared {
		p.Phases.Scope(RepositoryScope(item.Repository, index+1, len(prepared)))
		if err := p.validatePreparedTarget(ctx, item.Repository, item.Target, item.OID, slotID, preparePhaseCreate, item.locked.root, item.locked.relative, item.locked.identity, "validate remaining checkout"); err != nil {
			return nil, err
		}
		// 共有できる tracked file は checkout せず main から clone する。
		// checkout してから同内容へ差し替えるのに比べ、同じ bytes の書き出しと読み比べが1往復ぶん要らない。
		var placement cowPlacement
		if err := p.timePhase("cow-place", func() error {
			var placeErr error
			placement, placeErr = p.placeSharedFiles(ctx, item.Repository, item, slotID)
			return placeErr
		}); err != nil {
			return nil, err
		}
		if err := p.timePhase("checkout", func() error { return p.checkoutStage(ctx, item, false, placement.placed) }); err != nil {
			return nil, err
		}
		if err := p.verifyPreparedLFS(item.locked.root, item.locked.relative, item.Repository); err != nil {
			return nil, err
		}
		if err := p.timePhase("cow-verify", func() error { return p.settleCOWPlacement(ctx, item, placement.placed) }); err != nil {
			return nil, err
		}
		// post-checkout より前に実体化する。ユーザーの hook が submodule を前提にできるようにし、
		// hook 側の `git submodule update` も no-op で済ませるためである。
		var submoduleResult submodulePhaseResult
		if err := p.timePhase("submodule", func() error {
			var err error
			submoduleResult, err = p.submodulePhaseWithResult(ctx, item.Repository, item.Target, item.OID, item.locked.identity)
			return err
		}); err != nil {
			return nil, err
		}
		if err := p.timePhase("post-checkout", func() error {
			return p.runPostCheckoutWithSubmodules(ctx, item.Repository, item.Target, item.locked.identity, item.OID, submoduleResult)
		}); err != nil {
			return nil, err
		}
		if err := p.completePrepare(ctx, item.Repository, item.Target, item.OID, slotID, preparePhaseCreate, item.locked, placement, &submoduleResult,
			func() error { return nil },
			func() error { return nil },
			func() error { return p.materializePlan(ctx, item.Repository, item.locked, &item.plan, false) },
			func() error { return nil }); err != nil {
			return nil, err
		}
	}
	p.Phases.Scope(PhaseScope{})
	if rootStage != nil {
		if err := p.timePhase("root", func() error { return rootStage(false) }); err != nil {
			return nil, err
		}
	}
	placements := make(map[string][]state.Placement, len(prepared))
	for _, item := range prepared {
		placements[string(item.Repository.ID)] = item.plan.placements()
	}
	return placements, nil
}

func (p *Preparer) buildEarlyPlan(ctx context.Context, item *stagedRepository) error {
	if _, err := p.RunGitInWorktree(ctx, item.Target, item.locked.identity, nil, nil, "read-tree", item.OID); err != nil {
		return err
	}
	if err := p.applySparseCheckout(ctx, item); err != nil {
		return err
	}
	// -v は各 entry の先頭に tag を足す。tag を読んで sparse 範囲外を除いたうえで、後続の判定は tag 抜きの 3 field で行う。
	result, err := p.RunGitInWorktree(ctx, item.Target, item.locked.identity, nil, nil, "ls-files", "--stage", "-v", "-z")
	if err != nil {
		return err
	}
	item.plan.symlinks = map[string]string{}
	item.plan.oids = map[string]string{}
	for _, entry := range strings.Split(result.Stdout, "\x00") {
		if entry == "" {
			continue
		}
		metadata, path, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(metadata)
		if !ok || len(fields) != 4 {
			return fmt.Errorf("invalid index entry")
		}
		tag := fields[0]
		fields = fields[1:]
		// skip-worktree が立つのは sparse 範囲外の path である。plan から除くことで展開・CoW 配置・
		// placement 記録・include の上書き判定がまとめて範囲内だけを見る。範囲外を checkout-index へ
		// 渡すと git が exit 1 にし、その時点までの path を書き出したまま準備が失敗する。
		if tag == "S" || tag == "s" {
			continue
		}
		if fields[0] == "160000" {
			item.plan.gitlinks = append(item.plan.gitlinks, path)
			continue
		}
		item.plan.tracked = append(item.plan.tracked, path)
		if cowShareableIndexMode(fields) {
			item.plan.oids[path] = fields[1]
		}
		if fields[0] == "120000" {
			blob, err := p.RunGitInWorktree(ctx, item.Target, item.locked.identity, nil, nil, "cat-file", "blob", fields[1])
			if err != nil {
				return err
			}
			item.plan.symlinks[path] = blob.Stdout
		}
	}
	if err := p.planIncludes(item.Repository, &item.plan); err != nil {
		return err
	}
	// 要求 OID で追跡するパスは、source の index から外れていても include で上書きしない。
	tracked := map[string]bool{}
	for _, path := range item.plan.tracked {
		tracked[path] = true
	}
	copies := item.plan.copies[:0]
	for _, entry := range item.plan.copies {
		if !tracked[entry.path] {
			copies = append(copies, entry)
		}
	}
	item.plan.copies = copies
	item.plan.split(p.Config.ReadinessForWorkspaceRepository(p.workspaceRootForRepository(item.Repository), item.Repository.RelativePath, string(item.Repository.MainPath)).EarlyPaths)
	return nil
}

// applySparseCheckout は read-tree が作った index へ、worktree が受け継いだ sparse 条件を反映する。
// `git worktree add` は sparse の設定自体を複製するが、プレーンな read-tree は条件を適用しないため、
// この一手を挟まないと設定と index が食い違ったまま範囲外まで展開される。
// reapply は範囲外の実体を削除するので、worktree がまだ空である read-tree 直後にだけ呼べる。
// sparse でない repository では reapply が失敗するため、事前に config で切り分ける。
// commentlint:allow-long -- 呼ぶ位置を誤ると範囲外の実体を消すため、前提と制約を明示する
func (p *Preparer) applySparseCheckout(ctx context.Context, item *stagedRepository) error {
	enabled, err := p.RunGitInWorktree(ctx, item.Target, item.locked.identity, nil, nil, "config", "--default", "false", "--type=bool", "--get", "core.sparseCheckout")
	if err != nil {
		return err
	}
	if strings.TrimSpace(enabled.Stdout) != "true" {
		return nil
	}
	_, err = p.RunGitInWorktree(ctx, item.Target, item.locked.identity, nil, nil, "sparse-checkout", "reapply")
	return err
}

// checkoutStage は plan のうち early 区分が一致する tracked path を展開する。
// placed は既に clone で配置済みの path で、再展開すると clone した実体を上書きするため除く。
func (p *Preparer) checkoutStage(ctx context.Context, item *stagedRepository, early bool, placed map[string]bool) error {
	for _, path := range item.plan.gitlinks {
		if item.plan.early[path] != early {
			continue
		}
		destination, err := domain.OpenRootAt(item.locked.root, item.locked.relative)
		if err != nil {
			return err
		}
		createErr := ensureRootDirectory(destination, path)
		_ = destination.Close()
		if createErr != nil {
			return createErr
		}
	}
	var paths []string
	for _, path := range item.plan.tracked {
		if item.plan.early[path] == early && !placed[path] {
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	// --force は使わず、先行配置後に現れた衝突を上書きせず失敗させる。
	// checkout.workers は Git 側の parallel checkout を有効にする。設定の既定は 1 で、repository 設定に依らず同じ並列度にするため毎回明示する。
	args := []string{"-c", "checkout.workers=" + strconv.Itoa(checkoutWorkers()), "checkout-index", "--index", "-z", "--stdin"}
	var env []string
	if item.plan.earlyAttributes() {
		env = []string{"GIT_ATTR_SOURCE=" + item.OID}
	}
	_, err := p.RunGitInWorktree(ctx, item.Target, item.locked.identity, env, []byte(strings.Join(paths, "\x00")+"\x00"), args...)
	return err
}

// checkoutMaxWorkers は parallel checkout の上限である。
// これを超える並列度は Git 側の同期費用が勝ち、手元の計測では実時間が伸びた。
const checkoutMaxWorkers = 10

// checkoutWorkers は checkout-index の並列度を返す。
func checkoutWorkers() int {
	return min(runtime.NumCPU(), checkoutMaxWorkers)
}
