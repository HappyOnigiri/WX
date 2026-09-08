package hookconfig

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Result は Install / Remove の適用結果である。
// Resolved は symlink を辿った実体で、dotfile リポジトリ側での commit を案内するために返す。
type Result struct {
	Path     string
	Resolved string
	Backup   string
	Changed  bool
	State    State
}

// Install は agent の設定ファイルへ wx 専用の hook group を書き、書いた直後に再検査した状態を返す。
// 冪等であり、既存の記録が同じ実体へ解決するなら表記が違っても書き換えない。
func Install(agent string) (Result, error) {
	binary, err := ResolveHookBinary()
	if err != nil {
		return Result{}, err
	}
	return applyHookConfig(agent, func(document *jsonNode) error { return installEntries(document, binary) })
}

// Remove は agent の設定ファイルから wx の hook エントリだけを取り除く。
// 他者の hook・group・event は残し、wx の除去で空になった器だけを畳む。
func Remove(agent string) (Result, error) {
	return applyHookConfig(agent, func(document *jsonNode) error {
		pruneEntries(document)
		return nil
	})
}

// applyHookConfig は設定ファイルを読み、edit を適用し、変化したときだけ atomic に書き戻す。
// 対象が regular file でない・JSON が読めない・現 uid 所有でない場合は書かずに失敗する。
func applyHookConfig(agent string, edit func(*jsonNode) error) (Result, error) {
	path, err := TargetPath(agent)
	if err != nil {
		return Result{}, err
	}
	result := Result{Path: path, Resolved: path}
	original, resolved, existed, err := readWritableTarget(path)
	if err != nil {
		return Result{}, err
	}
	result.Resolved = resolved
	document := &jsonNode{kind: jsonObject}
	if existed {
		document, err = decodeDocument(original)
		if err != nil {
			return Result{}, fmt.Errorf("%s: %w", path, err)
		}
		if document.kind != jsonObject {
			return Result{}, fmt.Errorf("%s: the top level of the hook configuration is not a JSON object", path)
		}
	}
	if err := edit(document); err != nil {
		return Result{}, err
	}
	rendered := renderDocument(document)
	if existed && bytes.Equal(rendered, original) {
		result.State, err = Inspect(agent)
		return result, err
	}
	if existed && agent == "claude" {
		if backup, err := backupHookConfig(original); err != nil {
			return Result{}, err
		} else if backup != "" {
			result.Backup = backup
		}
	}
	if err := writeHookConfig(resolved, existed, rendered); err != nil {
		return Result{}, err
	}
	result.Changed = true
	result.State, err = Inspect(agent)
	return result, err
}

// readWritableTarget は書き込み先の実体を確かめ、既存の内容を返す。
// symlink は辿って実体を書く。リンクの上に rename すると dotfile リポジトリとの接続が黙って切れるためである。
func readWritableTarget(path string) (data []byte, resolved string, existed bool, err error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, path, false, nil
	}
	if err != nil {
		return nil, "", false, err
	}
	resolved = path
	if info.Mode()&os.ModeSymlink != 0 {
		if resolved, err = filepath.EvalSymlinks(path); err != nil {
			return nil, "", false, fmt.Errorf("%s: %w", path, err)
		}
	}
	target, err := os.Stat(resolved)
	if err != nil {
		return nil, "", false, err
	}
	if !target.Mode().IsRegular() {
		return nil, "", false, fmt.Errorf("%s is not a regular file", resolved)
	}
	if !ownedByCurrentUser(target) {
		return nil, "", false, fmt.Errorf("%s is not owned by the current user", resolved)
	}
	data, err = os.ReadFile(resolved)
	if err != nil {
		return nil, "", false, err
	}
	switch {
	case len(data) == 0:
		// 読み側は 0 byte を拒否する。空ファイルを {} と見なすと、書いた直後に未登録と表示される。
		return nil, "", false, fmt.Errorf("%s is empty; remove it or restore valid JSON before configuring hooks", resolved)
	case len(data) > maxHookConfigSize:
		return nil, "", false, fmt.Errorf("%s is larger than the 4MiB limit the readiness check accepts", resolved)
	}
	return data, resolved, true, nil
}

// writeHookConfig は同じディレクトリの一時ファイルへ書いてから rename する。
// 既存ファイルの permission を引き継ぎ、新規は 0o600 で作る。
func writeHookConfig(resolved string, existed bool, data []byte) error {
	mode := os.FileMode(0o600)
	if existed {
		info, err := os.Stat(resolved)
		if err != nil {
			return err
		}
		mode = info.Mode().Perm()
	} else if err := os.MkdirAll(filepath.Dir(resolved), 0o700); err != nil {
		return err
	}
	directory := filepath.Dir(resolved)
	tmp, err := os.CreateTemp(directory, ".wx-hooks-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, resolved); err != nil {
		return err
	}
	handle, err := os.Open(directory)
	if err != nil {
		return err
	}
	err = handle.Sync()
	_ = handle.Close()
	return err
}

// backupHookConfig は claude の settings を 1 スロットだけ控える。
// ~/.claude 直下に settings*.json に一致する余計なファイルを増やさないため、wx の状態ディレクトリへ置く。
func backupHookConfig(original []byte) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	directory := filepath.Join(home, "Library", "Application Support", "wx", "backups")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(directory, "claude-settings.json")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// installEntries は Events() ごとに wx 専用 group を先頭へ置く。
// 他者の hook と同じ group には決して入れない。group 内に 1 つでも不正な hook があると group 丸ごと却下されるためである。
// 配列順の尊重は agent 実装依存なので、先頭に置くのは best-effort であり正しさの根拠にはしない。
func installEntries(document *jsonNode, binary string) error {
	hooks, ok := document.field("hooks")
	if !ok || hooks.kind != jsonObject {
		if ok && hooks.kind != jsonObject {
			return errors.New("the hooks entry is not a JSON object")
		}
		hooks = &jsonNode{kind: jsonObject}
		document.setField("hooks", hooks)
	}
	for _, event := range Events() {
		command, err := hookCommandFor(binary, event.Name)
		if err != nil {
			return err
		}
		list, ok := hooks.field(event.Name)
		if !ok {
			list = &jsonNode{kind: jsonArray}
			hooks.setField(event.Name, list)
		}
		if list.kind != jsonArray {
			return fmt.Errorf("the %s entry is not a JSON array", event.Name)
		}
		if managedGroupCurrent(list, event.Subcommand, binary) {
			continue
		}
		pruneEvent(list, event.Subcommand)
		list.items = append([]*jsonNode{managedGroup(command)}, list.items...)
	}
	return nil
}

// managedGroupCurrent は、既存の group に同じ実体へ解決する wx hook が単独で入っているかを返す。
// install.sh の更新は同じ path の inode を入れ替えるだけで、記録の書き換えを必要としない。
// ここで stale と判定すると、更新のたびに TUI が出るという最も目立つ退行になる。
func managedGroupCurrent(list *jsonNode, subcommand, binary string) bool {
	for _, group := range list.items {
		hooks, ok := group.field("hooks")
		if !ok || hooks.kind != jsonArray || len(hooks.items) != 1 {
			continue
		}
		if _, present := group.field("matcher"); present && !dedicatedMatcher(group) {
			continue
		}
		command, ok := hookCommandOf(hooks.items[0])
		if !ok || wxHookSubcommand(command) != subcommand {
			continue
		}
		if isExactWXHookCommandForExecutable(command, subcommand, binary) {
			return true
		}
	}
	return false
}

// dedicatedMatcher は matcher が全 event に適用される形かを返す。
func dedicatedMatcher(group *jsonNode) bool {
	matcher, ok := group.field("matcher")
	if !ok {
		return true
	}
	value, ok := matcher.stringValue()
	if !ok {
		return false
	}
	switch value {
	case "", "*", ".*", "^.*$", "^.+$":
		return true
	default:
		return false
	}
}

// managedGroup は wx 専用の group を作る。
// matcher は key ごと省略し、disabled / async / once / timeout などの任意項目も書かない。
// null を書くと group ごと、あるいは file 全体が却下される。
func managedGroup(command string) *jsonNode {
	hook := &jsonNode{kind: jsonObject}
	hook.setField("type", scalarNode("command"))
	hook.setField("command", scalarNode(command))
	group := &jsonNode{kind: jsonObject}
	group.setField("hooks", &jsonNode{kind: jsonArray, items: []*jsonNode{hook}})
	return group
}

// pruneEntries は document 全体から wx の hook を取り除き、空になった器を畳む。
func pruneEntries(document *jsonNode) {
	hooks, ok := document.field("hooks")
	if !ok || hooks.kind != jsonObject {
		return
	}
	for _, event := range Events() {
		list, ok := hooks.field(event.Name)
		if !ok || list.kind != jsonArray {
			continue
		}
		pruneEvent(list, event.Subcommand)
		if len(list.items) == 0 {
			// 空配列を残すと、他の設定と見分けが付かない痕跡になる。key ごと落とす。
			hooks.removeField(event.Name)
		}
	}
	if len(hooks.keys) == 0 {
		document.removeField("hooks")
	}
}

// pruneEvent は 1 つの event 配列から wx の hook 要素を取り除き、空になった group も落とす。
func pruneEvent(list *jsonNode, subcommand string) {
	kept := list.items[:0]
	for _, group := range list.items {
		hooks, ok := group.field("hooks")
		if !ok || hooks.kind != jsonArray {
			kept = append(kept, group)
			continue
		}
		keptHooks := hooks.items[:0]
		for _, hook := range hooks.items {
			command, ok := hookCommandOf(hook)
			if ok && wxHookSubcommand(command) == subcommand && namesWXExecutable(command) {
				continue
			}
			keptHooks = append(keptHooks, hook)
		}
		hooks.items = keptHooks
		if len(hooks.items) == 0 {
			continue
		}
		kept = append(kept, group)
	}
	list.items = kept
}

// hookCommandOf は hook 要素の command 文字列を返す。type が command でないものは対象外とする。
func hookCommandOf(hook *jsonNode) (string, bool) {
	kind, ok := hook.field("type")
	if !ok {
		return "", false
	}
	if value, ok := kind.stringValue(); !ok || value != "command" {
		return "", false
	}
	command, ok := hook.field("command")
	if !ok {
		return "", false
	}
	return command.stringValue()
}

// namesWXExecutable は command の先頭 token が wx を名指ししているかを返す。
// 実体が消えた記録も回収できるよう、実行中の wx との一致だけでなく basename も見る。
func namesWXExecutable(command string) bool {
	fields, ok := splitHookCommand(command)
	if !ok || len(fields) == 0 {
		return false
	}
	if filepath.Base(fields[0].value) == "wx" {
		return true
	}
	resolved, ok := resolveHookExecutable(fields[0].value)
	if !ok {
		return false
	}
	current, err := CurrentExecutable()
	return err == nil && sameExecutable(resolved, current)
}
