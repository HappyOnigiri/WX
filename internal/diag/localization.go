package diag

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/HappyOnigiri/WX/internal/i18n"
)

// LocalizeReply は daemon が返す診断本文を表示言語へ変換する。
// check・severity・target と、外部コマンド由来の値はそのままにし、CLI が
// JSON を要求しない RPC の人間向け応答だけが日本語になるよう Reply を複製する。
func LocalizeReply(reply Reply, lang i18n.Language) Reply {
	if lang != i18n.Japanese {
		return reply
	}
	out := reply
	out.Findings = make([]Finding, len(reply.Findings))
	for index, finding := range reply.Findings {
		localized := finding
		localized.Summary = diagnosticText(finding.Summary, lang)
		localized.Cause = diagnosticText(finding.Cause, lang)
		localized.Action = diagnosticText(finding.Action, lang)
		if finding.Details != nil {
			localized.Details = make([]string, len(finding.Details))
			for detailIndex, detail := range finding.Details {
				localized.Details[detailIndex] = diagnosticText(detail, lang)
			}
		}
		out.Findings[index] = localized
	}
	return out
}

// RenderLanguage は doctor の人間向け表示を指定言語へ解決する。
// check・target とその値、外部コマンドのエラー本文は機械値・原文として残し、
// wx が生成した要約・原因の前後だけを置き換える。Render は旧 caller 用の英語 API である。
func RenderLanguage(w io.Writer, reply Reply, verbose bool, lang i18n.Language) {
	findings := visible(reply, verbose)
	if !hasReportable(findings) {
		_, _ = fmt.Fprintln(w, diagnosticText("No errors found.", lang))
	}
	for index, finding := range findings {
		if index > 0 {
			_, _ = fmt.Fprintln(w)
		}
		renderFindingLanguage(w, finding, verbose, lang)
	}
}

func renderFindingLanguage(w io.Writer, finding Finding, verbose bool, lang i18n.Language) {
	rows := [][2]string{
		{severityLabel(finding.Severity), diagnosticText(finding.Summary, lang)},
		{"check", finding.Check},
		{"target", finding.Target},
		{"cause", diagnosticText(finding.Cause, lang)},
		{"action", diagnosticText(finding.Action, lang)},
	}
	if verbose {
		for _, detail := range finding.Details {
			rows = append(rows, [2]string{"detail", diagnosticText(detail, lang)})
		}
	}
	width := 0
	for _, row := range rows {
		if row[1] != "" && len(row[0]) > width {
			width = len(row[0])
		}
	}
	for _, row := range rows {
		if row[1] == "" {
			continue
		}
		label := diagnosticLabel(row[0], lang)
		_, _ = fmt.Fprintf(w, "%-*s %s\n", width+1, label+":", singleLine(row[1]))
	}
}

func diagnosticLabel(value string, lang i18n.Language) string {
	if lang != i18n.Japanese {
		return value
	}
	switch value {
	case "error":
		return "エラー"
	case "unchecked":
		return "未検査"
	case "note":
		return "注記"
	case "ok":
		return "正常"
	case "unknown":
		return "不明"
	case "check":
		return "検査"
	case "target":
		return "対象"
	case "cause":
		return "原因"
	case "action":
		return "対処"
	case "detail":
		return "詳細"
	default:
		return value
	}
}

var diagnosticReplacements = []struct{ en, ja string }{
	{"No errors found.", "エラーはありません。"},
	{"the wx daemon could not be reached", "wx daemon に接続できません"},
	{"the daemon is running read-only because its SQLite state is unavailable", "SQLite の状態を利用できないため daemon は読み取り専用で動作しています"},
	{"the configuration is loaded", "設定を読み込みました"},
	{"the configuration could not be loaded", "設定を読み込めませんでした"},
	{"fix the reported entry in the configuration file, then run wx config reload", "設定ファイルの報告された項目を修正し、wx config reload を実行してください"},
	{"git could not be executed", "git を実行できません"},
	{"install Git or fix PATH and the executable permission of git, then run wx doctor again", "Git をインストールするか PATH と git の実行権限を修正し、wx doctor を再実行してください"},
	{"git is available", "git を利用できます"},
	{"the daemon socket path could not be resolved", "daemon socket の path を解決できません"},
	{"the daemon socket is not usable", "daemon socket を利用できません"},
	{"the daemon socket does not exist; the daemon creates it when it starts", "daemon socket はまだ存在しません。daemon 起動時に作成されます"},
	{"run wx daemon start if you expect the daemon to be running", "daemon の起動を想定している場合は wx daemon start を実行してください"},
	{"run wx daemon start; if it is not installed as a LaunchAgent, run wx daemon install", "wx daemon start を実行してください。LaunchAgent として未登録なら wx daemon install を実行してください"},
	{"remove or fix the reported path so the daemon can bind its own socket, then run wx daemon start", "daemon が socket を bind できるよう報告された path を削除または修正し、wx daemon start を実行してください"},
	{"the state database path could not be resolved", "状態データベースの path を解決できません"},
	{"the state database file is not usable", "状態データベースのファイルを利用できません"},
	{"the state database does not exist yet; the daemon creates it when it starts", "状態データベースはまだ存在しません。daemon 起動時に作成されます"},
	{"the LaunchAgent content check is deferred", "LaunchAgent の内容検査は延期されています"},
	{"a daemon restart is pending, and the running process cannot render the plist of the replacement binary", "daemon の再起動が保留中で、実行中の process は置き換え後 binary の plist を生成できません"},
	{"run wx doctor again once the replacement daemon is up", "置き換え後の daemon が起動したら wx doctor を再実行してください"},
	{"the wx LaunchAgent is not installed", "wx LaunchAgent がインストールされていません"},
	{"the LaunchAgent plist does not exist, so the daemon does not start on login", "LaunchAgent plist が存在しないため、login 時に daemon が起動しません"},
	{"run wx daemon install", "wx daemon install を実行してください"},
	{"the wx LaunchAgent is not usable", "wx LaunchAgent を利用できません"},
	{"run wx daemon install to rewrite the plist with owner-only access", "owner-only access の plist を書き直すため wx daemon install を実行してください"},
	{"the LaunchAgent plist matches this binary", "LaunchAgent plist はこの binary と一致します"},
	{"the LaunchAgent plist does not match this binary", "LaunchAgent plist はこの binary と一致しません"},
	{"the installed plist differs from the one this wx binary renders, so login starts a different daemon", "インストール済み plist はこの wx binary が生成するものと異なるため、login では別の daemon が起動します"},
	{"the LaunchAgent plist could not be compared", "LaunchAgent plist を比較できません"},
	{"the installed plist could not be compared with the one this wx binary renders", "インストール済み plist とこの wx binary が生成する plist を比較できません"},
	{"the plist comparison returned an unknown status", "plist の比較結果が不明です"},
	{"run wx daemon install to reinstall the plist from this binary", "この binary の plist を再インストールするため wx daemon install を実行してください"},
	{"the configured worktree root could not be resolved", "設定された worktree root を解決できません"},
	{"fix storage.worktree_root in the configuration file, then run wx config reload", "設定ファイルの storage.worktree_root を修正し、wx config reload を実行してください"},
	{"the worktree root does not exist yet; wx creates it when it registers the root", "worktree root はまだ存在しません。root 登録時に wx が作成します"},
	{"run wx daemon start if you expect slots to be prepared", "slot の準備を想定している場合は wx daemon start を実行してください"},
	{"readiness hooks are configured", "readiness hook が設定されています"},
	{"readiness hooks are missing or invalid", "readiness hook がないか不正です"},
	{"the agent has no valid wx readiness hooks, so wx waits for readiness in the foreground", "agent に有効な wx readiness hook がないため、wx は foreground で準備完了を待ちます"},
	{"no action is required; configure the hooks only to skip the foreground wait", "対処は不要です。foreground 待機を省く場合だけ hook を設定してください"},
	{"the path is usable", "path は利用できます"},
	{"the path does not exist", "path は存在しません"},
	{"this check did not run", "この検査は実行されませんでした"},
	{"the SQLite state of the wx daemon was unavailable, so this check could not run", "wx daemon の SQLite 状態を利用できなかったため、この検査は実行されませんでした"},
	{"fix the failure this check depends on, then run wx doctor again", "この検査が依存する失敗を修正し、wx doctor を再実行してください"},
	{"preserve ", "保全: "},
	{"restore a verified backup from ", "検証済みバックアップから復元: "},
	{"then restart the daemon", "その後 daemon を再起動してください"},
	{"stop the daemon and remove ", "daemon を停止して削除: "},
	{"wx creates the current layout on the next start", "次回起動時に wx が現在のレイアウトを作成します"},
	{"the workspace probe did not run", "workspace 検査は実行されませんでした"},
	{"start the wx daemon, then run wx doctor --probe again", "wx daemon を起動してから wx doctor --probe を再実行してください"},
	{"the registered workspaces could not be read from the daemon", "登録済み workspace を daemon から読み取れません"},
	{"fix the reported daemon failure, then run wx doctor --probe again", "報告された daemon の失敗を修正し、wx doctor --probe を再実行してください"},
	{"no workspace was probed", "workspace は検査されませんでした"},
	{"no registered workspace uses a wx worktree, so there was nothing to prepare and check", "wx worktree を使う登録済み workspace がないため、準備・検査するものがありません"},
	{"no action is required; run wx new or an agent command in a repository to register one", "対処は不要です。登録するには repository で wx new または agent command を実行してください"},
	{"retire standby", "standby を回収"},
	{"lease:", "貸出:"},
	{"early ready:", "早期準備完了:"},
	{"full ready:", "準備完了:"},
	{"the daemon returned no diagnostics", "daemon は診断結果を返しませんでした"},
	{"which wx doctor can read, but its reply carried no check result at all", "wx doctor が読める schema ですが、応答に検査結果がありません"},
	{"check the daemon log for the failed reply, then run wx doctor again", "失敗した応答を daemon log で確認し、wx doctor を再実行してください"},
	{"and wx doctor needs schema ", "wx doctor には schema "},
	{"or newer to read its results", "以上が必要です"},
	{"run wx daemon restart so the daemon runs this wx binary, then run wx doctor again", "この wx binary で daemon を動かすため wx daemon restart を実行し、wx doctor を再実行してください"},
	{"check that HOME points to your home directory, then run wx doctor again", "HOME がホームディレクトリを指すことを確認し、wx doctor を再実行してください"},
	{"remove or fix the reported path so the daemon can bind its own socket, then run wx daemon start", "daemon が socket を bind できるよう報告された path を削除または修正し、wx daemon start を実行してください"},
	{"restore the file type and its 0600 owner-only access; keep the current file for investigation instead of deleting it", "ファイル種別と 0600 の owner-only access を復元し、削除せず現在のファイルを調査用に保全してください"},
	{"fix the path, its 0700 owner-only access, or the mount it lives on; wx retries the registration on each reconcile", "path、0700 の owner-only access、または配置先 mount を修正してください。wx は reconcile ごとに登録を再試行します"},
	{"path unavailable", "path を利用できません"},
	{"unsafe symlink", "安全でない symlink"},
	{"not a directory", "directory ではありません"},
	{"not a Unix socket", "Unix socket ではありません"},
	{"not a regular file", "通常ファイルではありません"},
	{"unsafe permissions ", "安全でない権限 "},
	{"the prepared worktree shares blocks with the main worktrees", "準備した worktree は main worktree と block を共有しています"},
	{"the prepared worktree shares no block with the main worktrees", "準備した worktree は main worktree と block を共有していません"},
	{"no file of the slot prepared for ", "準備した slot（"},
	{" was found to share blocks with its main worktree, so the copy took the full size", "）には main worktree と block を共有するファイルがなく、コピーは全サイズになりました"},
	{"no action is required; check that the worktree root and the repositories live on the same APFS volume if you expect CoW sharing", "対処は不要です。CoW 共有を想定する場合は worktree root と repository が同じ APFS volume にあることを確認してください"},
	{"the preparation wrote output without failing", "準備は失敗せずに出力を残しました"},
	{"the output was truncated for this report", "この report では出力を省略しました"},
	{"full output in ", "完全な出力: "},
	{"no action is required unless the output reports a failure the hook swallowed; run wx doctor --probe -v to read it", "hook が隠した失敗を出力が示していない限り対処は不要です。読むには wx doctor --probe -v を実行してください"},
	{"a workspace could not be leased for the probe", "probe 用に workspace を貸し出せませんでした"},
	{"fix the reported cause; wx cannot hand this workspace to an agent until then", "報告された原因を修正してください。それまで wx はこの workspace を agent に渡せません"},
	{"a workspace could not be prepared for the probe", "probe 用に workspace を準備できませんでした"},
	{"read the reported detail log for the failing command, fix its cause, then run wx doctor --probe again", "失敗した command の detail log を読み、原因を修正して wx doctor --probe を再実行してください"},
}

func diagnosticText(text string, lang i18n.Language) string {
	if lang != i18n.Japanese || text == "" {
		return text
	}
	// prepareNoticeFindings の本文には phase 名と workspace root が動的に入る。
	// それらを置換表へ登録すると path や外部由来の値まで翻訳してしまうため、
	// 固定の前後だけを組み替えて動的な値はそのまま残す。
	const (
		phaseMarker = " phase of the preparation for "
		phaseSuffix = " finished without failing but wrote output"
	)
	if strings.HasPrefix(text, "the ") {
		rest := strings.TrimPrefix(text, "the ")
		if marker := strings.Index(rest, phaseMarker); marker > 0 {
			afterMarker := rest[marker+len(phaseMarker):]
			if suffix := strings.Index(afterMarker, phaseSuffix); suffix >= 0 {
				phase := rest[:marker]
				root := afterMarker[:suffix]
				tail := afterMarker[suffix+len(phaseSuffix):]
				text = "準備の " + phase + " phase（" + root + "）は失敗せずに出力を残しました" + tail
			}
		}
	}
	replacements := append([]struct{ en, ja string }(nil), diagnosticReplacements...)
	sort.SliceStable(replacements, func(i, j int) bool { return len(replacements[i].en) > len(replacements[j].en) })
	for _, replacement := range replacements {
		text = strings.ReplaceAll(text, replacement.en, replacement.ja)
	}
	return text
}
