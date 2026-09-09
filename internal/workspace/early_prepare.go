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
// 呼び出し元は slot lock を保持し、開始を永続化しておく。失敗時も部分展開を削除せず残す。
// rootStage は非 Git workspace root の配置、earlyReady は全先行配置の永続化を受け持つ。
func (p *Preparer) PrepareStaged(ctx context.Context, slotID string, repositories []Preparation, rootStage func(bool) error, earlyReady func() error) error {
	stagedPreparer := *p
	stagedPreparer.noCheckout = true
	p = &stagedPreparer
	var prepared []*stagedRepository
	defer func() {
		for _, repo := range prepared {
			repo.locked.close()
		}
	}()
	for _, request := range repositories {
		root, target, err := p.prepareTarget(request.Target)
		if err != nil {
			return err
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
			return err
		}
		prepared = append(prepared, item)
		if item.locked.existing {
			return fmt.Errorf("%w: staged preparation target already exists", state.ErrOwnership)
		}
		if err := p.timePhase("early-index", func() error { return p.buildEarlyPlan(ctx, item) }); err != nil {
			return err
		}
		if err := p.timePhase("early-checkout", func() error { return p.checkoutStage(ctx, item, true, nil) }); err != nil {
			return err
		}
		if err := p.timePhase("early-place", func() error {
			return p.materializePlan(ctx, item.Repository, item.locked, &item.plan, true)
		}); err != nil {
			return err
		}
	}
	if rootStage != nil {
		if err := p.timePhase("early-root", func() error { return rootStage(true) }); err != nil {
			return err
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
		return err
	}
	for _, item := range prepared {
		if err := p.validatePreparedTarget(ctx, item.Repository, item.Target, item.OID, slotID, preparePhaseCreate, item.locked.root, item.locked.relative, item.locked.identity, "validate remaining checkout"); err != nil {
			return err
		}
		// 共有できる tracked file は checkout せず main から clone する。
		// checkout してから同内容へ差し替えるのに比べ、同じ bytes の書き出しと読み比べが1往復ぶん要らない。
		var placed map[string]bool
		if err := p.timePhase("cow-place", func() error {
			var placeErr error
			placed, placeErr = p.placeSharedFiles(ctx, item.Repository, item, slotID)
			return placeErr
		}); err != nil {
			return err
		}
		p.sharedPlaced = len(placed) > 0
		if err := p.timePhase("checkout", func() error { return p.checkoutStage(ctx, item, false, placed) }); err != nil {
			return err
		}
		if err := p.timePhase("cow-verify", func() error { return p.settleCOWPlacement(ctx, item, placed) }); err != nil {
			return err
		}
		// worktree add の post-checkout と同じ null OID・新 HEAD・branch flag を使う。
		// Git 自身に hook 選択と実行を任せ、未配置の相対 hooksPath も全展開後に解決する。
		if err := p.timePhase("post-checkout", func() error {
			_, err := p.RunGitInWorktree(ctx, item.Target, item.locked.identity, nil, nil, "hook", "run", "--ignore-missing", "post-checkout", "--", strings.Repeat("0", len(item.OID)), item.OID, "1")
			return err
		}); err != nil {
			return err
		}
		if err := p.completePrepare(ctx, item.Repository, item.Target, item.OID, slotID, preparePhaseCreate, item.locked,
			func() error { return p.materializePlan(ctx, item.Repository, item.locked, &item.plan, false) },
			func() error { return nil }); err != nil {
			return err
		}
	}
	if rootStage != nil {
		return p.timePhase("root", func() error { return rootStage(false) })
	}
	return nil
}

func (p *Preparer) buildEarlyPlan(ctx context.Context, item *stagedRepository) error {
	if _, err := p.RunGitInWorktree(ctx, item.Target, item.locked.identity, nil, nil, "read-tree", item.OID); err != nil {
		return err
	}
	result, err := p.RunGitInWorktree(ctx, item.Target, item.locked.identity, nil, nil, "ls-files", "--stage", "-z")
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
		if !ok || len(fields) != 3 {
			return fmt.Errorf("invalid index entry")
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
	item.plan.split(p.Config.Readiness.EarlyPaths)
	return nil
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
