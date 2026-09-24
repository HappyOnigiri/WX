package setup

import (
	"testing"

	"github.com/HappyOnigiri/WorktreeX/internal/i18n"
)

// TestMessageBuildsTemplateData は、名前と値を交互に並べる書き方が template のデータになることと、
// 名前だけが余った呼び出しでもその名前を捨てることを確認する。
// 値の欠けた名前を template へ渡すと、表示に <no value> が出る。
func TestMessageBuildsTemplateData(t *testing.T) {
	value := message("setup.reason.startup_unreadable", "Path", "/home/user/.zshrc", "Error", "permission denied")
	if got := englishText(value); got != "/home/user/.zshrc could not be read: permission denied" {
		t.Fatalf("resolved=%q", got)
	}
	if plain := message("setup.reason.home_unresolved"); plain.Data != nil {
		t.Fatalf("a message without fields carried data: %+v", plain.Data)
	}
	if odd := message("setup.reason.home_unresolved", "Path"); len(odd.Data) != 0 {
		t.Fatalf("an unpaired name reached the template data: %+v", odd.Data)
	}
}

// TestMessageKeepsOneCompletePair は、値が 1 組だけの呼び出しでも template data を作ることを確認する。
// ここを空の引数と同じ扱いにすると、要約や理由の単一フィールドが表示できなくなる。
func TestMessageKeepsOneCompletePair(t *testing.T) {
	value := message("setup.summary.shell_path_block", "Directory", "/home/user/.local/bin")
	if len(value.Data) != 1 || value.Data["Directory"] != "/home/user/.local/bin" {
		t.Fatalf("one pair was not retained: %+v", value.Data)
	}
	empty := message("setup.summary.shell_path_block", "Directory", "")
	if len(empty.Data) != 1 || empty.Data["Directory"] != "" {
		t.Fatalf("an empty value was not retained as a complete pair: %+v", empty.Data)
	}
	if got := englishText(value); got != "adds /home/user/.local/bin to PATH for new terminals" {
		t.Fatalf("resolved=%q", got)
	}
	odd := message("setup.summary.shell_path_block", "Directory", "/home/user/.local/bin", "orphan")
	if len(odd.Data) != 1 || odd.Data["Directory"] != "/home/user/.local/bin" {
		t.Fatalf("an unpaired trailing name changed the complete pair: %+v", odd.Data)
	}
}

// TestMessageErrorKeepsItsIdentity は、適用の失敗が message ID を持つ error として返り、
// 表示言語で解決できることを確認する。ID を失うと日本語設定でも英語のまま表示される。
func TestMessageErrorKeepsItsIdentity(t *testing.T) {
	err := messageError("setup.error.daemon_rejected_config", "Path", "/home/user/config.yaml", "Error", "busy")
	if got := i18n.LocalizeError(err, i18n.English); got != "/home/user/config.yaml was saved but the running daemon rejected it: busy" {
		t.Fatalf("english=%q", got)
	}
	if got := i18n.LocalizeError(err, i18n.Japanese); got == i18n.LocalizeError(err, i18n.English) {
		t.Fatalf("the japanese text did not differ from the english one: %q", got)
	}
}

// TestPathProblemFallsBackToTheRawResult は、diag が message を持たない判定（Lstat の失敗）で
// 原文が本文として残ることを確認する。ここで message を要求すると、理由が空のまま表示される。
func TestPathProblemFallsBackToTheRawResult(t *testing.T) {
	raw := pathProblem("/home/user/wx", "no such file or directory", i18n.Message{})
	if got := englishText(raw); got != "/home/user/wx: no such file or directory" {
		t.Fatalf("raw=%q", got)
	}
	known := pathProblem("/home/user/wx", "not a directory", i18n.Message{ID: "diag.path.not_directory"})
	if got := englishText(known); got != "/home/user/wx: not a directory" {
		t.Fatalf("known=%q", got)
	}
}
