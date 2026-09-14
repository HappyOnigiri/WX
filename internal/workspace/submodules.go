package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
)

// submodulePhaseResult は submodule の実体化結果を post-checkout hook へ渡すための一時値である。
// 省略した子を hook が再初期化しないよう、宣言順の適格判定も保持する。
type submodulePhaseResult struct {
	enabled   bool
	declared  []submodule
	decisions []submoduleProbe
}

// submodulePhase は submodule 実体化を prepare の1区間として呼ぶ入口である。
// 方針が無効な workspace では Git を1回も起動しない。
func (p *Preparer) submodulePhase(ctx context.Context, repo discovery.Repository, target, oid, identity string) error {
	_, err := p.submodulePhaseWithResult(ctx, repo, target, oid, identity)
	return err
}

func (p *Preparer) submodulePhaseWithResult(ctx context.Context, repo discovery.Repository, target, oid, identity string) (submodulePhaseResult, error) {
	enabled, err := p.submodulesEnabled(repo)
	if err != nil {
		return submodulePhaseResult{}, err
	}
	if !enabled {
		return submodulePhaseResult{}, nil
	}
	p.SubmoduleOutcomes.BeginRepository(string(repo.MainPath))
	return p.materializeSubmodulesWithResult(ctx, repo, target, oid, identity)
}

// submodulesEnabled は repository の属する workspace で submodule を実体化するかを解決する。
func (p *Preparer) submodulesEnabled(repo discovery.Repository) (bool, error) {
	root := p.workspaceRootForRepository(repo)
	if root == "" {
		return false, fmt.Errorf("resolve workspace root for repository %s", repo.MainPath)
	}
	resolved := p.Config.RepositoryFor(root, repo.RelativePath, string(repo.MainPath))
	if resolved.Submodules != nil {
		return *resolved.Submodules, nil
	}
	enabled, _ := p.Config.SubmodulesForWorkspace(root)
	return enabled, nil
}

// submodule は要求 OID の .gitmodules と index から確定した1件の submodule である。
// name は main の `.git/modules/<name>` を指し、path は worktree 上の配置先を指す。
// 両者は一致しないので、module directory の組み立てには必ず name、worktree 操作には必ず path を使う。
type submodule struct {
	name string
	path string
	url  string
	oid  string
}

// materializedSubmodule は実体化と origin 復元に必要な値を適格判定後に固定した1件である。
type materializedSubmodule struct {
	module   submodule
	source   string
	upstream string
}

type submoduleProbeInput struct {
	index  int
	module submodule
}

// materializeSubmodules は要求 OID の submodule を worktree へ実体化する。
// linked worktree では Git が submodule の gitdir を per-worktree の `$GIT_DIR/modules/<name>` に解決し、main の
// `.git/modules/<name>` を再利用できないため、そのローカル module から clone してネットワークを使わずに済ませる。
// ローカルに必要な object が無い場合は書き込む前に省略して warn を残し、準備自体は成功させる。
// commentlint:allow-long -- linked worktree で毎回ネットワーク clone に落ちる理由と、省略して成功させる方針の根拠を残す
func (p *Preparer) materializeSubmodules(ctx context.Context, repo discovery.Repository, target, oid, identity string) error {
	_, err := p.materializeSubmodulesWithResult(ctx, repo, target, oid, identity)
	return err
}

// materializeSubmodulesWithResult は materializeSubmodules と同じ処理を行い、hook 用の判定結果も返す。
func (p *Preparer) materializeSubmodulesWithResult(ctx context.Context, repo discovery.Repository, target, oid, identity string) (submodulePhaseResult, error) {
	if err := ctx.Err(); err != nil {
		return submodulePhaseResult{}, err
	}
	stats := &submoduleStats{}
	defer stats.recordSubmodulePhases(p.Phases)
	workers := p.submoduleWorkers()
	declared, err := p.declaredSubmodules(ctx, target, oid, identity)
	if err != nil {
		return submodulePhaseResult{}, err
	}
	stats.declared.Store(int64(len(declared)))
	commonModules := filepath.Join(string(repo.CommonDir), "modules")
	decisions := make([]submoduleProbe, len(declared))
	phaseResult := submodulePhaseResult{enabled: true, declared: declared, decisions: decisions}
	var candidates []submoduleProbeInput
	for index, module := range declared {
		if module.url == "" {
			// url が無い entry は clone 後に origin を戻す先が無いため実体化しない。
			decisions[index] = submoduleProbe{
				skipMessage: "submodule has no url in .gitmodules",
				skipArgs:    []any{"repository", string(repo.MainPath), "submodule", module.name},
				skipReason:  SubmoduleReasonURLMissing,
			}
			continue
		}
		source := filepath.Join(commonModules, module.name)
		if !domain.IsWithin(commonModules, source) {
			return submodulePhaseResult{}, fmt.Errorf("submodule %q resolves outside the source repository module directory", module.name)
		}
		candidates = append(candidates, submoduleProbeInput{index: index, module: module})
	}
	if len(candidates) == 0 {
		for index, module := range declared {
			decisions[index].log(p, module)
			p.recordSubmoduleOutcome(repo, module, SubmoduleActionSkipped, decisions[index].skipReason)
			stats.skipped.Add(1)
		}
		return phaseResult, nil
	}

	// source module の読み取りだけを worker pool へ渡し、warn は宣言順にまとめて出す。
	probeBatches := make([][]submoduleProbeInput, 0, len(candidates))
	for _, candidate := range candidates {
		probeBatches = append(probeBatches, []submoduleProbeInput{candidate})
	}
	probeErr := runCOWBatches(ctx, workers, probeBatches, func(ctx context.Context, batch []submoduleProbeInput) error {
		input := batch[0]
		module := input.module
		source := filepath.Join(commonModules, module.name)
		start := time.Now()
		decision, err := p.inspectSubmoduleUpstream(ctx, source, module)
		stats.inspect.observe(start)
		if err != nil {
			return err
		}
		decisions[input.index] = decision
		return nil
	})
	if probeErr != nil {
		return submodulePhaseResult{}, probeErr
	}
	var eligible []materializedSubmodule
	for index, module := range declared {
		decision := decisions[index]
		if !decision.eligible {
			decision.log(p, module)
			p.recordSubmoduleOutcome(repo, module, SubmoduleActionSkipped, decision.skipReason)
			stats.skipped.Add(1)
			continue
		}
		decision.log(p, module)
		eligible = append(eligible, materializedSubmodule{module: module, source: filepath.Join(commonModules, module.name), upstream: decision.upstream})
	}
	stats.eligible.Store(int64(len(eligible)))
	phaseResult.decisions = decisions
	if len(eligible) == 0 {
		return phaseResult, nil
	}

	materializeBatches := batchSubmoduleMaterialization(eligible, workers)
	for _, batch := range materializeBatches {
		start := time.Now()
		_, updateErr := p.RunGitInWorktree(ctx, target, identity, nil, nil, submoduleUpdateArgs(batch, workers)...)
		stats.materialize.observe(start)
		if updateErr != nil {
			return submodulePhaseResult{}, fmt.Errorf("materialize submodules: %w", updateErr)
		}
	}

	// 各子の config は独立しているので、clone 後の origin 復元だけを worker pool へ渡す。
	originBatches := make([][]materializedSubmodule, 0, len(eligible))
	for _, module := range eligible {
		originBatches = append(originBatches, []materializedSubmodule{module})
	}
	if err := runCOWBatches(ctx, workers, originBatches, func(ctx context.Context, batch []materializedSubmodule) error {
		module := batch[0]
		start := time.Now()
		_, err := p.RunGitInWorktree(ctx, target, identity, nil, nil, "-C", module.module.path, "remote", "set-url", "origin", module.upstream)
		stats.origin.observe(start)
		if err != nil {
			return fmt.Errorf("restore submodule %s origin: %w", module.module.path, err)
		}
		return nil
	}); err != nil {
		return submodulePhaseResult{}, err
	}
	// 実体化は batch 単位で進むため、origin 復元まで通った時点で宣言順にまとめて記録する。
	for _, module := range eligible {
		p.recordSubmoduleOutcome(repo, module.module, SubmoduleActionMaterialized, "")
	}
	return phaseResult, nil
}

// Submodule は rev の `.gitmodules` と index から確定した submodule 1 件の公開表現である。
// Name は source repository の `<common>/modules/<name>` を指し、Path は worktree 上の配置先を指す。
// OID は index の gitlink で、rev の tree の値ではない。貸出中に親が gitlink を進めた結果を見るためである。
type Submodule struct {
	Name string
	Path string
	OID  string
}

// Submodules は rev の `.gitmodules` に宣言され、index に gitlink がある submodule を返す。
// prepare の実体化と、返却時の未保全検出が同じ列挙を通るようにするための入口である。
// `.gitmodules` を持たない rev では Git を1回起動して空を返す。
func (p *Preparer) Submodules(ctx context.Context, target, rev, identity string) ([]Submodule, error) {
	modules, err := p.declaredSubmodules(ctx, target, rev, identity)
	if err != nil {
		return nil, err
	}
	out := make([]Submodule, 0, len(modules))
	for _, module := range modules {
		out = append(out, Submodule{Name: module.name, Path: module.path, OID: module.oid})
	}
	return out, nil
}

// SubmodulesAtRevision は source repository の rev の tree から submodule を列挙する。
// doctor は worktree の index を持たないため、.gitmodules の候補 path だけを ls-tree へ渡す。
func (p *Preparer) SubmodulesAtRevision(ctx context.Context, repository, rev string) ([]Submodule, error) {
	declared, err := p.declaredSubmodulesAtTree(ctx, repository, rev)
	if err != nil {
		return nil, err
	}
	return p.resolveSubmoduleOIDsFromTree(ctx, repository, rev, declared)
}

func (p *Preparer) recordSubmoduleOutcome(repo discovery.Repository, module submodule, action SubmoduleAction, reason string) {
	if p.SubmoduleOutcomes == nil {
		return
	}
	p.SubmoduleOutcomes.Add(SubmoduleOutcome{
		Repository: string(repo.MainPath), Path: module.path, Depth: 1, Action: action, Reason: reason,
	})
}

// declaredSubmodules は rev の .gitmodules と index から submodule を確定する。
// .gitmodules を持たない repository はここで終わり、追加の Git 起動は1回だけになる。
func (p *Preparer) declaredSubmodules(ctx context.Context, target, rev, identity string) ([]submodule, error) {
	if !gitProbe(p.RunGitInWorktree(ctx, target, identity, nil, nil, "cat-file", "-e", rev+":.gitmodules")) {
		return nil, nil
	}
	// 要求 OID の .gitmodules を worktree の実体に依らず読む。設定は checkout 前でも blob から解決できる。
	entries, err := p.RunGitInWorktree(ctx, target, identity, nil, nil, "config", "--blob", rev+":.gitmodules", "--get-regexp", `^submodule\.`)
	if err != nil {
		if isEmptySubmoduleConfig(ctx, err) {
			return nil, nil
		}
		return nil, err
	}
	declared, err := parseSubmoduleConfig(entries.Stdout)
	if err != nil {
		return nil, err
	}
	var candidates []submodule
	for _, module := range declared {
		if module.path == "" {
			continue
		}
		candidates = append(candidates, module)
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	return p.resolveSubmoduleOIDs(ctx, target, identity, candidates)
}

// declaredSubmodulesAtTree は rev の .gitmodules から候補を作る。
// source repository の現在 index を読まないので、doctor は要求 OID と同じ tree を診断できる。
func (p *Preparer) declaredSubmodulesAtTree(ctx context.Context, repository, rev string) ([]submodule, error) {
	if !gitProbe(p.Git.Run(ctx, repository, "cat-file", "-e", rev+":.gitmodules")) {
		return nil, nil
	}
	entries, err := p.Git.Run(ctx, repository, "config", "--blob", rev+":.gitmodules", "--get-regexp", `^submodule\.`)
	if err != nil {
		if isEmptySubmoduleConfig(ctx, err) {
			return nil, nil
		}
		return nil, err
	}
	declared, err := parseSubmoduleConfig(entries.Stdout)
	if err != nil {
		return nil, err
	}
	var candidates []submodule
	for _, module := range declared {
		if module.path != "" {
			candidates = append(candidates, module)
		}
	}
	return candidates, nil
}

// isEmptySubmoduleConfig は Git が設定検索の結果なしを返した場合だけ空定義と判定する。
// context の中断や出力を伴う失敗は、実行障害・構文エラーとして呼び出し側へ返す。
func isEmptySubmoduleConfig(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var gitErr *gitx.Error
	return errors.As(err, &gitErr) && gitErr.Result.ExitCode == 1 && gitErr.Result.Stdout == "" && gitErr.Result.Stderr == ""
}

// resolveSubmoduleOIDs は候補 path の index entry から gitlink OID を引く。
// `ls-tree -r` は大 repository で全 tree を走査するため使わず、候補 path だけを index に問い合わせる。
func (p *Preparer) resolveSubmoduleOIDs(ctx context.Context, target, identity string, candidates []submodule) ([]submodule, error) {
	base := []string{"ls-files", "--stage", "-z", "--"}
	gitlinks := map[string]string{}
	for _, batch := range batchSubmoduleArgs(candidates, base, func(module submodule) []string { return []string{module.path} }) {
		args := append([]string(nil), base...)
		for _, module := range batch {
			args = append(args, module.path)
		}
		staged, err := p.RunGitInWorktree(ctx, target, identity, nil, nil, args...)
		if err != nil {
			return nil, err
		}
		for _, entry := range strings.Split(staged.Stdout, "\x00") {
			if entry == "" {
				continue
			}
			metadata, path, ok := strings.Cut(entry, "\t")
			fields := strings.Fields(metadata)
			if !ok || len(fields) != 3 {
				return nil, fmt.Errorf("invalid index entry")
			}
			if fields[0] != "160000" {
				continue
			}
			gitlinks[filepath.Clean(path)] = fields[1]
		}
	}
	var modules []submodule
	for _, module := range candidates {
		oid, ok := gitlinks[module.path]
		if !ok {
			// .gitmodules に残っているが index に gitlink が無い entry は、この OID では submodule ではない。
			continue
		}
		module.oid = oid
		modules = append(modules, module)
	}
	return modules, nil
}

// resolveSubmoduleOIDsFromTree は候補 path だけを要求 OID の tree へ問い合わせる。
func (p *Preparer) resolveSubmoduleOIDsFromTree(ctx context.Context, repository, rev string, candidates []submodule) ([]Submodule, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	base := []string{"ls-tree", "-z", rev, "--"}
	gitlinks := map[string]string{}
	for _, batch := range batchSubmoduleArgs(candidates, base, func(module submodule) []string { return []string{module.path} }) {
		args := append([]string(nil), base...)
		for _, module := range batch {
			args = append(args, module.path)
		}
		tree, err := p.Git.Run(ctx, repository, args...)
		if err != nil {
			return nil, err
		}
		for _, entry := range strings.Split(tree.Stdout, "\x00") {
			if entry == "" {
				continue
			}
			metadata, path, ok := strings.Cut(entry, "\t")
			fields := strings.Fields(metadata)
			if !ok || len(fields) != 3 {
				return nil, fmt.Errorf("invalid tree entry")
			}
			if fields[0] == "160000" {
				gitlinks[filepath.Clean(path)] = fields[2]
			}
		}
	}
	modules := make([]Submodule, 0, len(candidates))
	for _, module := range candidates {
		oid, ok := gitlinks[module.path]
		if !ok {
			continue
		}
		modules = append(modules, Submodule{Name: module.name, Path: module.path, OID: oid})
	}
	return modules, nil
}

// parseSubmoduleConfig は `git config --get-regexp` の出力から submodule の name/path/url を組み立てる。
// name は `modules/autoscaler` のように `/` や `.` を含み得るため、キー全体から先頭 `submodule.` と
// 末尾の属性名だけを剥がして name を取る。先頭側から区切ると nested name を壊す。
func parseSubmoduleConfig(output string) ([]submodule, error) {
	byName := map[string]*submodule{}
	var order []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		name, attribute, ok := splitSubmoduleKey(key)
		if !ok {
			continue
		}
		module, seen := byName[name]
		if !seen {
			module = &submodule{name: name}
			byName[name] = module
			order = append(order, name)
		}
		switch attribute {
		case "path":
			clean, err := safeRelative(value)
			if err != nil {
				return nil, fmt.Errorf("unsafe submodule path %q: %w", value, err)
			}
			module.path = clean
		case "url":
			module.url = value
		}
	}
	modules := make([]submodule, 0, len(order))
	for _, name := range order {
		module := byName[name]
		if module.path == "" && module.url == "" {
			continue
		}
		// name は main の module directory 名になるため、`..` や絶対 path を含む値を作らせない。
		clean, err := safeRelative(module.name)
		if err != nil {
			return nil, fmt.Errorf("unsafe submodule name %q: %w", module.name, err)
		}
		if clean != module.name || hasGitDirComponent(module.name) {
			return nil, fmt.Errorf("unsafe submodule name %q", module.name)
		}
		modules = append(modules, *module)
	}
	return modules, nil
}

// hasGitDirComponent は path 成分に `.git` そのものを含むかを返す。
// module directory 名として使うため、Git 管理ディレクトリと衝突する成分を拒否する。
// 判定は成分単位で行う。部分一致にすると `libs/mylib.github` のような正当な名前まで準備失敗にしてしまう。
func hasGitDirComponent(name string) bool {
	for _, component := range strings.Split(name, string(filepath.Separator)) {
		if strings.EqualFold(component, ".git") {
			return true
		}
	}
	return false
}

// splitSubmoduleKey は `submodule.<name>.<attribute>` を name と attribute に分ける。
func splitSubmoduleKey(key string) (string, string, bool) {
	rest, ok := strings.CutPrefix(key, "submodule.")
	if !ok {
		return "", "", false
	}
	index := strings.LastIndex(rest, ".")
	if index <= 0 || index == len(rest)-1 {
		return "", "", false
	}
	return rest[:index], rest[index+1:], true
}

// submoduleUpstreamWithReason は main のローカル module が clone 元として使えるかを判定し、clone 後に戻す origin と理由を返す。
// 戻す origin は .gitmodules の url ではなくローカル module の `remote.origin.url` を使う。
// .gitmodules の url は `../child` のような相対表記があり、その解決は superproject の remote 基準になるため、
// ここで再実装すると Git と食い違う。ローカル module の origin は Git 自身が解決した結果である。
// commentlint:allow-long -- .gitmodules の相対 url を自前解決しない理由を保守時に確認できるようにする
func (p *Preparer) submoduleUpstream(ctx context.Context, source string, module submodule) (string, bool) {
	decision, err := p.inspectSubmoduleUpstream(ctx, source, module)
	if err != nil {
		p.logSkip("submodule local module could not be inspected", "submodule", module.name, "module_dir", source, "error", err)
		return "", false
	}
	decision.log(p, module)
	return decision.upstream, decision.eligible
}

// submoduleProbe は source module の読み取り結果と、後で出す warn の内容を持つ。
// 検査を並列に行っても、ログは宣言順に出せるように書込みを遅らせる。
type submoduleProbe struct {
	upstream    string
	eligible    bool
	skipMessage string
	skipArgs    []any
	// skipReason は省略を SubmoduleOutcomes へ記録するための機械向け識別子である。
	skipReason string
	source     string
	shallow    bool
}

func (s submoduleProbe) log(p *Preparer, module submodule) {
	if s.shallow && p.Log != nil {
		p.Log.Warn("submodule object sharing is unavailable because the local module is shallow", "submodule", module.name, "module_dir", s.source)
	}
	if !s.eligible {
		if s.skipMessage != "" {
			p.logSkip(s.skipMessage, s.skipArgs...)
		}
		return
	}
}

func (p *Preparer) inspectSubmoduleUpstream(ctx context.Context, source string, module submodule) (submoduleProbe, error) {
	if err := ctx.Err(); err != nil {
		return submoduleProbe{}, err
	}
	info, statErr := os.Stat(source)
	if statErr != nil || !info.IsDir() {
		//nolint:nilerr // local module不在は検査失敗ではなく想定した省略である。
		return submoduleProbe{
			skipMessage: "submodule has no local module in the source repository",
			skipArgs:    []any{"submodule", module.name, "module_dir", source},
			skipReason:  SubmoduleReasonLocalModule,
		}, nil
	}
	inspection, inspectErr := InspectSubmodule(ctx, p.Git, source, module.oid)
	if inspectErr != nil {
		if ctx.Err() != nil || errors.Is(inspectErr, context.Canceled) || errors.Is(inspectErr, context.DeadlineExceeded) {
			return submoduleProbe{}, inspectErr
		}
		return submoduleProbe{
			skipMessage: "submodule local module could not be inspected",
			skipArgs:    []any{"submodule", module.name, "module_dir", source, "error", inspectErr},
			skipReason:  SubmoduleReasonInspectionFailed,
		}, nil
	}
	// promisor で要求 OID が無いまま clone すると checkout が書込み後に失敗するため、
	// shallow でない通常 module の欠落 OID と同じく、書き込む前に省略する。
	if inspection.Status() == SubmoduleSharingPromisorMissing {
		return submoduleProbe{
			skipMessage: "submodule object is missing from the promisor local module",
			skipArgs:    []any{"submodule", module.name, "oid", module.oid},
			skipReason:  SubmoduleReasonObjectMissing,
		}, nil
	}
	if inspection.Status() == SubmoduleSharingObjectMissing {
		// gitlink OID がローカル module に無いまま clone すると親が ` M <path>` の dirty で残り、
		// その gitdir は wx が消せない場所にできる。書き込む前にここで弾く。
		return submoduleProbe{
			skipMessage: "submodule commit is missing from the local module",
			skipArgs:    []any{"submodule", module.name, "oid", module.oid},
			skipReason:  SubmoduleReasonObjectMissing,
		}, nil
	}
	if inspection.OriginURL == "" {
		return submoduleProbe{
			skipMessage: "submodule local module has no origin url",
			skipArgs:    []any{"submodule", module.name, "module_dir", source},
			skipReason:  SubmoduleReasonOriginMissing,
			source:      source,
			shallow:     inspection.Status() == SubmoduleSharingShallow,
		}, nil
	}
	return submoduleProbe{upstream: inspection.OriginURL, eligible: true, source: source, shallow: inspection.Status() == SubmoduleSharingShallow}, nil
}

// gitProbe は Git の終了状態だけを見て前置き検査の結果を返す。
// object や commit の有無を確かめる用途で、失敗は「無い」と同じに扱い error として伝播させない。
func gitProbe(_ gitx.Result, err error) bool { return err == nil }
