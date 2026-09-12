package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/i18n"
)

func cliLanguage(c Client) i18n.Language { return i18n.Normalize(c.Config.DisplayLanguage()) }

func cliErrorPrefix(lang i18n.Language) string {
	if lang == i18n.Japanese {
		return "エラー:"
	}
	return "error:"
}

func cliError(c Client, err error) {
	lang := cliLanguage(c)
	fmt.Fprintln(os.Stderr, cliErrorPrefix(lang), localizeCLIMessage(err.Error(), lang))
}

func localizeCLIMessage(text string, lang i18n.Language) string {
	if lang != i18n.Japanese {
		return text
	}
	for _, replacement := range []struct{ en, ja string }{
		{"--workspace and --repository cannot be combined", "--workspace と --repository は併用できません"},
		{"wx run needs a command after --", "wx run には -- の後にコマンドが必要です"},
		{"--runs must be at least 1", "--runs は 1 以上で指定してください"},
		{"--config and --sweep cannot be combined with --reuse; each configuration is measured as a cold start", "--config と --sweep は --reuse と併用できません。各設定は cold start として計測します"},
		{"cannot resolve a wx workspace from ", "次の場所から wx workspace を解決できません: "},
		{"; run wx bench --reuse to measure without retiring standby worktrees", "。standby worktree を回収せずに計測するには wx bench --reuse を実行してください"},
		{" is configured not to use a worktree; change worktree.undefined or the workspace policy ", " は worktree を使わない設定です。worktree.undefined または workspace policy を変更してください "},
		{"policy saved but daemon reload failed: ", "方針は保存しましたが daemon の再読み込みに失敗しました: "},
		{"reload worktree policy:", "worktree 方針の再読み込み:"},
		{"lease root identity changed (expected ", "lease root の identity が変わりました（期待値 "},
		{"resolve workspace history: ", "workspace history を解決できません: "},
		{"; run wx daemon restart if the daemon has not been updated", "。daemon が更新されていなければ wx daemon restart を実行してください"},
		{"no conversation found for this workspace", "この workspace に会話が見つかりません"},
		{"unknown resume intent", "不明な resume 指示です"},
		{"--fresh requires a resume operation", "--fresh には resume 操作が必要です"},
		{"--branch requires --fresh when resuming", "resume 時の --branch には --fresh が必要です"},
		{"--branch and --fresh require a worktree", "--branch と --fresh には worktree が必要です"},
		{"wx session ", "wx session "},
		{" holds a lease, not an agent conversation; use wx shell --resume ", " は lease を保持しており agent の会話ではありません。wx shell --resume "},
		{"create operation identity", "操作識別子を作成"},
		{"pin workspace CWD", "workspace の CWD を固定"},
		{"locate wx descriptor helper", "wx descriptor helper を見つける"},
		{"prepare agent", "agent を準備"},
		{"register agent process", "agent process を登録"},
		{"reload worktree policy", "worktree 方針を再読み込み"},
		{"wx daemon is reachable but this request did not complete", "wx daemon には接続できますが、要求を完了できませんでした"},
		{"wx daemon is unavailable", "wx daemon は利用できません"},
		{"wx daemon did not become ready", "wx daemon の準備が完了しませんでした"},
		{"run wx doctor", "wx doctor を実行してください"},
		{"LaunchAgent plist is stale; run wx daemon install", "LaunchAgent plist が古いため wx daemon install を実行してください"},
		{"interrupted before the workspace was leased", "workspace の貸出前に中断されました"},
		{"interrupted while the workspace was being prepared; releasing it", "workspace の準備中に中断されました。貸出を返却します"},
		{"workspace preparation", "workspace の準備"},
		{"retire standby", "standby を回収"},
		{"lease:", "貸出:"},
		{"early ready:", "早期準備完了:"},
		{"full ready:", "準備完了:"},
		{"lease cancelled; no workspace was created", "貸出をキャンセルしました。workspace は作成されませんでした"},
		{"launch cancelled; no workspace was created", "起動をキャンセルしました。workspace は作成されませんでした"},
		{"wx clear asked this session to stop before the agent started", "agent の起動前に wx clear から停止要求を受けました"},
		{"selected conversation has no working directory", "選択した会話に作業ディレクトリがありません"},
		{"worktree policy requires a terminal; use wx --worktree or wx --no-worktree, or configure worktree.undefined", "worktree の方針選択には端末が必要です。wx --worktree / wx --no-worktree を使うか worktree.undefined を設定してください"},
		{"the daemon is already stopping; wait for it to exit and run wx daemon start", "daemon は停止処理中です。終了を待って wx daemon start を実行してください"},
		{"the daemon is already restarting; run the command again once the replacement is up", "daemon は再起動処理中です。置き換え後にもう一度実行してください"},
	} {
		text = strings.ReplaceAll(text, replacement.en, replacement.ja)
	}
	return text
}

func reportLeaseErrorLanguage(err error, lang i18n.Language) int {
	fmt.Fprintln(os.Stderr, cliErrorPrefix(lang), localizeCLIMessage(err.Error(), lang))
	if daemon.IsWorktreeDisabled(err) {
		return 2
	}
	return 1
}
