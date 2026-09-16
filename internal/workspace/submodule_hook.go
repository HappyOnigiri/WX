package workspace

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
)

// runPostCheckoutWithSubmodules は submodule 段階の判定を一時的な -c 設定として hook へ渡す。
// wx が省略した子の再初期化を防ぎ、source repository の共有 config を hook から変更させない。
func (p *Preparer) runPostCheckoutWithSubmodules(ctx context.Context, repo discovery.Repository, target, identity, oid string, submodules submodulePhaseResult) error {
	if !submodules.enabled || len(submodules.declared) == 0 {
		return p.runPostCheckout(ctx, target, identity, oid)
	}
	args := []string{}
	if submodules.enabled {
		eligible := 0
		for _, decision := range submodules.decisions {
			if decision.eligible {
				eligible++
			}
		}
		if eligible > 0 {
			args = append(args, "-c", "protocol.file.allow=always")
			for index, module := range submodules.declared {
				if index < len(submodules.decisions) && submodules.decisions[index].eligible {
					args = append(args, "-c", "submodule.active=:(top,literal)"+module.path)
				}
			}
		} else if len(submodules.declared) > 0 {
			// 宣言はあるが全件を省略した場合も、hook の無指定 update が子を初期化しないよう全 path を除外する。
			args = append(args, "-c", "submodule.active=:(top,exclude)**")
		}
		commonModules := filepath.Join(string(repo.CommonDir), "modules")
		for index, module := range submodules.declared {
			if index >= len(submodules.decisions) {
				break
			}
			decision := submodules.decisions[index]
			if decision.eligible {
				source := filepath.Join(commonModules, module.name)
				if !domain.IsWithin(commonModules, source) {
					return fmt.Errorf("submodule %q resolves outside the source repository module directory", module.name)
				}
				// hook 内の `git submodule update` にも URL と active を継承させ、Git の自動補完が共有 config へ書かないようにする。
				args = append(args, "-c", "submodule."+module.name+".url="+source,
					"-c", "submodule."+module.name+".active=true")
				continue
			}
			// wx が書込み前に省略した子を hook の再帰 update が初期化しないようにする。
			args = append(args, "-c", "submodule."+module.name+".active=false")
		}
	}
	args = append(args, "hook", "run", "--ignore-missing", "post-checkout", "--", strings.Repeat("0", len(oid)), oid, "1")
	result, err := p.RunGitInWorktree(ctx, target, identity, nil, nil, args...)
	if err != nil {
		// 失敗した hook の出力は gitx が failure ID 付きの detail log へ既に書いている。
		// notice にも積むと同じ出力が 2 つの log に分かれ、失敗を「失敗せずに出力した」として報告することになる。
		return err
	}
	// exit 0 の hook が出した出力も残す。hook が内部の失敗を飲み込むと、捨てた時点で wx からは正常と区別できなくなる。
	p.Notices.Add(PrepareNotice{
		Target: target, Phase: "post-checkout", Stdout: result.Stdout, Stderr: result.Stderr,
	})
	return nil
}
