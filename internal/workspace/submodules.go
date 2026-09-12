package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
)

// submodulePhase は submodule 実体化を prepare の1区間として呼ぶ入口である。
// 方針が無効な workspace では Git を1回も起動しない。
func (p *Preparer) submodulePhase(ctx context.Context, repo discovery.Repository, target, oid, identity string) error {
	enabled, err := p.submodulesEnabled(repo)
	if err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	return p.materializeSubmodules(ctx, repo, target, oid, identity)
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

// materializeSubmodules は要求 OID の submodule を worktree へ実体化する。
// linked worktree では Git が submodule の gitdir を per-worktree の `$GIT_DIR/modules/<name>` に解決し、main の
// `.git/modules/<name>` を再利用できないため、そのローカル module から clone してネットワークを使わずに済ませる。
// ローカルに必要な object が無い場合は書き込む前に省略して warn を残し、準備自体は成功させる。
// commentlint:allow-long -- linked worktree で毎回ネットワーク clone に落ちる理由と、省略して成功させる方針の根拠を残す
func (p *Preparer) materializeSubmodules(ctx context.Context, repo discovery.Repository, target, oid, identity string) error {
	modules, err := p.planSubmodules(ctx, repo, target, oid, identity)
	if err != nil {
		return err
	}
	commonModules := filepath.Join(string(repo.CommonDir), "modules")
	for _, module := range modules {
		source := filepath.Join(commonModules, module.name)
		if !domain.IsWithin(commonModules, source) {
			return fmt.Errorf("submodule %q resolves outside the source repository module directory", module.name)
		}
		upstream, eligible := p.submoduleUpstream(ctx, source, module)
		if !eligible {
			continue
		}
		if err := p.cloneSubmodule(ctx, target, identity, module, source, upstream); err != nil {
			return err
		}
	}
	return nil
}

// planSubmodules は要求 OID の submodule を列挙する。
// .gitmodules を持たない repository はここで終わり、追加の Git 起動は1回だけになる。
func (p *Preparer) planSubmodules(ctx context.Context, repo discovery.Repository, target, oid, identity string) ([]submodule, error) {
	if !gitProbe(p.RunGitInWorktree(ctx, target, identity, nil, nil, "cat-file", "-e", oid+":.gitmodules")) {
		return nil, nil
	}
	// 要求 OID の .gitmodules を worktree の実体に依らず読む。設定は checkout 前でも blob から解決できる。
	entries, err := p.RunGitInWorktree(ctx, target, identity, nil, nil, "config", "--blob", oid+":.gitmodules", "--get-regexp", `^submodule\.`)
	if err != nil {
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
		if module.url == "" {
			// url が無い entry は clone 後に origin を戻す先が無いため実体化しない。
			p.logSkip("submodule has no url in .gitmodules", "repository", string(repo.MainPath), "submodule", module.name)
			continue
		}
		candidates = append(candidates, module)
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	return p.resolveSubmoduleOIDs(ctx, target, identity, candidates)
}

// resolveSubmoduleOIDs は候補 path の index entry から gitlink OID を引く。
// `ls-tree -r` は大 repository で全 tree を走査するため使わず、候補 path だけを index に問い合わせる。
func (p *Preparer) resolveSubmoduleOIDs(ctx context.Context, target, identity string, candidates []submodule) ([]submodule, error) {
	args := []string{"ls-files", "--stage", "-z", "--"}
	for _, module := range candidates {
		args = append(args, module.path)
	}
	staged, err := p.RunGitInWorktree(ctx, target, identity, nil, nil, args...)
	if err != nil {
		return nil, err
	}
	gitlinks := map[string]string{}
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

// submoduleUpstream は main のローカル module が clone 元として使えるかを判定し、clone 後に戻す origin を返す。
// 戻す origin は .gitmodules の url ではなくローカル module の `remote.origin.url` を使う。
// .gitmodules の url は `../child` のような相対表記があり、その解決は superproject の remote 基準になるため、
// ここで再実装すると Git と食い違う。ローカル module の origin は Git 自身が解決した結果である。
// commentlint:allow-long -- .gitmodules の相対 url を自前解決しない理由を保守時に確認できるようにする
func (p *Preparer) submoduleUpstream(ctx context.Context, source string, module submodule) (string, bool) {
	info, statErr := os.Stat(source)
	if statErr != nil || !info.IsDir() {
		p.logSkip("submodule has no local module in the source repository", "submodule", module.name, "module_dir", source)
		return "", false
	}
	// gitlink OID がローカルに無いまま clone すると親が ` M <path>` の dirty で残り、
	// その gitdir は wx が消せない場所にできる。書き込む前にここで弾く。
	if !gitProbe(p.Git.Run(ctx, source, "--git-dir=.", "cat-file", "-e", module.oid+"^{commit}")) {
		p.logSkip("submodule commit is missing from the local module", "submodule", module.name, "oid", module.oid)
		return "", false
	}
	origin, originErr := p.Git.Run(ctx, source, "--git-dir=.", "config", "--get", "remote.origin.url")
	upstream := strings.TrimSpace(origin.Stdout)
	if originErr != nil || upstream == "" {
		p.logSkip("submodule local module has no origin url", "submodule", module.name, "module_dir", source)
		return "", false
	}
	return upstream, true
}

// gitProbe は Git の終了状態だけを見て前置き検査の結果を返す。
// object や commit の有無を確かめる用途で、失敗は「無い」と同じに扱い error として伝播させない。
func gitProbe(_ gitx.Result, err error) bool { return err == nil }

// cloneSubmodule はローカル module から submodule を実体化し、origin を上流へ戻す。
// clone と origin の復元を分けないのは、origin がローカル module を指したまま残ると `git push` が
// main の `.git/modules` に入るためである。set-url の失敗は準備失敗にして中途半端な origin を残さない。
func (p *Preparer) cloneSubmodule(ctx context.Context, target, identity string, module submodule, source, upstream string) error {
	// config は必ず `-c` で渡す。gitx の環境サニタイズが GIT_CONFIG_* を落とし、repo-local config は子の clone プロセスに効かない。
	// protocol.file.allow はローカル path からの clone を許可するために必要である。
	args := []string{
		"-c", "protocol.file.allow=always",
		"-c", "submodule." + module.name + ".url=" + source,
		"submodule", "update", "--init", "--", module.path,
	}
	if _, err := p.RunGitInWorktree(ctx, target, identity, nil, nil, args...); err != nil {
		return fmt.Errorf("materialize submodule %s: %w", module.path, err)
	}
	// RunGitInWorktree は worktree root に fchdir で束縛するため、submodule 内で動かすには `-C` を渡す。
	if _, err := p.RunGitInWorktree(ctx, target, identity, nil, nil, "-C", module.path, "remote", "set-url", "origin", upstream); err != nil {
		return fmt.Errorf("restore submodule %s origin: %w", module.path, err)
	}
	return nil
}
