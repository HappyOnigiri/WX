package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/hookconfig"
	"github.com/HappyOnigiri/WX/internal/launchd"
)

// RemoveResult は 1 項目の削除結果である。Note は消した実体や控えの path のように、項目名から読み取れない事実だけを持つ。
type RemoveResult struct {
	ID   string
	Note string
	Err  error
}

// Removal は wx setup --remove の結果である。
// Leftovers は wx が作ったが削除しない path で、保存済みの作業や記録を含むため消すかどうかは利用者が決める。
type Removal struct {
	Results   []RemoveResult
	Leftovers []string
}

// Failed は 1 項目でも削除に失敗したかを返す。
func (r Removal) Failed() bool {
	for _, result := range r.Results {
		if result.Err != nil {
			return true
		}
	}
	return false
}

// Remove は wx setup が書き込んだ設定を消す。
// shell 起動ファイルは対象外である。dotfile 管理下の起動ファイルや利用者自身が書いた PATH 行と区別できないため、
// PATH 行は消さずに案内だけを残す。
// 順序は hook エントリ → LaunchAgent → config.yaml で固定する。
// LaunchAgent の解除は daemon を bootout するため、daemon 越しの片付け（wx clear など）はこの呼び出しより前に終えておく必要がある。
// commentlint:allow-long -- shell 起動ファイルを対象外にする理由と、daemon が落ちる副作用の順序制約は呼び出し側が取り違えると復旧できない
func Remove(ctx context.Context, options Options) Removal {
	removal := Removal{}
	for _, agent := range []string{"claude", "codex"} {
		removal.Results = append(removal.Results, removeHooks(agent))
	}
	removal.Results = append(removal.Results, removeLaunchAgent(ctx, options))
	// worktree root は config.yaml が権威なので、削除より前に Leftovers を集める。
	removal.Leftovers = leftoverPaths()
	removal.Results = append(removal.Results, removeConfigFile())
	return removal
}

// removeHooks は agent hook 設定から wx のエントリを消す。
// 判定は agent が PATH にあるかではなく設定ファイルの有無で行う。agent を先に消した利用者の設定ファイルにも wx のエントリは残る。
// 設定ファイルが無いときに hookconfig.Remove を呼ぶと空の設定ファイルを新規作成してしまうため、ここで打ち切る。
func removeHooks(agent string) RemoveResult {
	result := RemoveResult{ID: "hooks." + agent}
	path, err := hookconfig.TargetPath(agent)
	if err != nil {
		result.Err = err
		return result
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		result.Note = "no hook configuration at " + path
		return result
	}
	removed, err := hookconfig.Remove(agent)
	if err != nil {
		result.Err = err
		return result
	}
	result.Note = hookApplyNote(removed)
	return result
}

// removeLaunchAgent は plist がある場合だけ解除する。
// 未登録の plist に launchctl bootout を当てると `Boot-out failed: 5: Input/output error` になり、
// launchd が「service が無い」と判定できる文言と区別できないため、--remove の 2 回目が失敗する。
func removeLaunchAgent(ctx context.Context, options Options) RemoveResult {
	result := RemoveResult{ID: stepLaunchAgent}
	path, err := launchd.PlistPath()
	if err != nil {
		result.Err = err
		return result
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		result.Note = "no LaunchAgent at " + path
		return result
	}
	if options.UninstallLaunchAgent == nil {
		result.Err = errors.New("removing the LaunchAgent is not available here")
		return result
	}
	if err := options.UninstallLaunchAgent(ctx); err != nil {
		result.Err = err
		return result
	}
	result.Note = "removed " + path
	return result
}

// removeConfigFile は config.yaml を消す。key 単位ではなくファイルごと消すのは、
// storage.worktree_root のように書き込んだ key を消す公開 API が config に無く、消し残しが divergent として残るためである。
func removeConfigFile() RemoveResult {
	result := RemoveResult{ID: "config"}
	path, err := config.Path()
	if err != nil {
		result.Err = err
		return result
	}
	switch err := os.Remove(path); {
	case err == nil:
		result.Note = "deleted " + path
	case errors.Is(err, os.ErrNotExist):
	default:
		result.Err = err
	}
	return result
}

// leftoverPaths は wx が作ったが --remove では消さない path を、実在するものだけ重複なく返す。
// 状態 DB・ログ・worktree root には保存済みの作業や記録が残るため、消すかどうかは利用者が決める。
// state.db とログはファイル単位で作るが、案内は付随物（state.db.backups・run・回転ログ）を含む親ディレクトリで出す。
func leftoverPaths() []string {
	var paths []string
	for _, resolve := range []func() (string, error){stateDirectory, logDirectory, effectiveWorktreeRoot} {
		path, err := resolve()
		if err != nil {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if !slices.Contains(paths, path) {
			paths = append(paths, path)
		}
	}
	return paths
}

func stateDirectory() (string, error) {
	path, err := config.StatePath()
	if err != nil {
		return "", err
	}
	return filepath.Dir(path), nil
}

func logDirectory() (string, error) {
	path, err := config.LogPath()
	if err != nil {
		return "", err
	}
	return filepath.Dir(path), nil
}

// effectiveWorktreeRoot は config を読んで実効の worktree root を返す。
func effectiveWorktreeRoot() (string, error) {
	raw, err := config.LoadRaw()
	if err != nil {
		return "", err
	}
	return config.ExpandHome(config.Merge(config.Defaults(), raw).Storage.WorktreeRoot)
}
