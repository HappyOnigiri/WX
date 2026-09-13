package discovery

import (
	"context"
	"errors"
	"strings"

	"github.com/HappyOnigiri/WX/internal/gitx"
)

// resolveDefaultBranch は明示設定のない repository について、手元の Git
// 参照だけから貸出の起点 branch を決める。remote への問い合わせは行わず、
// 得られた候補も貸出時と同じ ResolveRef で検証する。
func (d *Discoverer) resolveDefaultBranch(ctx context.Context, mainPath string) (string, error) {
	// origin/HEAD は最も強い証拠なので、remote 一覧を先に読む余分な Git 起動を避ける。
	// symbolic-ref が値を返した場合は origin が存在するとみなし、古い参照であっても
	// 別 remote の HEAD へ勝手に切り替えない。
	if branch, present, err := d.symbolicRemoteHead(ctx, mainPath, "origin"); err != nil {
		return "", err
	} else if present {
		if ok, err := d.defaultBranchCandidate(ctx, mainPath, branch); err != nil {
			return "", err
		} else if ok {
			return branch, nil
		}
	} else {
		remotes, err := d.remoteNames(ctx, mainPath)
		if err != nil {
			return "", err
		}
		// origin が無い場合だけ、remote が一つに決まるときにその HEAD を使う。
		if len(remotes) == 1 && remotes[0] != "origin" {
			if branch, present, err := d.symbolicRemoteHead(ctx, mainPath, remotes[0]); err != nil {
				return "", err
			} else if present {
				if ok, err := d.defaultBranchCandidate(ctx, mainPath, branch); err != nil {
					return "", err
				} else if ok {
					return branch, nil
				}
			}
		}
	}

	for _, branch := range []string{"main", "master"} {
		if ok, err := d.defaultBranchCandidate(ctx, mainPath, branch); err != nil {
			return "", err
		} else if ok {
			return branch, nil
		}
	}
	branch, present, err := d.singleLocalBranch(ctx, mainPath)
	if err != nil {
		return "", err
	}
	if present {
		if ok, err := d.defaultBranchCandidate(ctx, mainPath, branch); err != nil {
			return "", err
		} else if ok {
			return branch, nil
		}
	}
	return "", nil
}

// symbolicRemoteHead は refs/remotes/<remote>/HEAD の symbolic target を branch 名へ変換する。
// ref 不在は証拠がないだけなので空値で返し、それ以外の Git 障害は呼び出し元へ返す。
func (d *Discoverer) symbolicRemoteHead(ctx context.Context, mainPath, remote string) (string, bool, error) {
	res, err := d.Git.Run(ctx, mainPath, "symbolic-ref", "--quiet", "--short", "refs/remotes/"+remote+"/HEAD")
	if err != nil {
		if isMissingRefError(err) {
			return "", false, nil
		}
		return "", false, err
	}
	value := strings.TrimSpace(res.Stdout)
	prefix := remote + "/"
	if !strings.HasPrefix(value, prefix) {
		return "", false, nil
	}
	branch := strings.TrimPrefix(value, prefix)
	if branch == "" || strings.ContainsAny(branch, "\r\n\x00") {
		return "", false, nil
	}
	return branch, true, nil
}

func (d *Discoverer) remoteNames(ctx context.Context, mainPath string) ([]string, error) {
	res, err := d.Git.Run(ctx, mainPath, "remote")
	if err != nil {
		return nil, err
	}
	var remotes []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		name := strings.TrimSpace(line)
		if name != "" {
			remotes = append(remotes, name)
		}
	}
	return remotes, nil
}

func (d *Discoverer) singleLocalBranch(ctx context.Context, mainPath string) (string, bool, error) {
	res, err := d.Git.Run(ctx, mainPath, "for-each-ref", "--format=%(refname:strip=2)", "--count=2", "refs/heads")
	if err != nil {
		return "", false, err
	}
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	if len(lines) != 1 || lines[0] == "" {
		return "", false, nil
	}
	return lines[0], true, nil
}

func (d *Discoverer) defaultBranchCandidate(ctx context.Context, mainPath, branch string) (bool, error) {
	if branch == "" {
		return false, nil
	}
	_, ok, err := gitx.ResolveRef(ctx, d.Git, mainPath, branch)
	return ok, err
}

func isMissingRefError(err error) bool {
	var gitErr *gitx.Error
	return errors.As(err, &gitErr) && gitErr.Result.ExitCode == 1 && gitErr.Result.Stderr == ""
}
