package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/HappyOnigiri/WorktreeX/internal/daemon"
	"github.com/HappyOnigiri/WorktreeX/internal/i18n"
	"github.com/HappyOnigiri/WorktreeX/internal/rpc"
	"github.com/HappyOnigiri/WorktreeX/internal/tui"
	"github.com/HappyOnigiri/WorktreeX/internal/update"
)

// updateNoticeTimeout は案内のための RPC を待つ上限である。
// daemon が持つ記録を読むだけなので、起動をこれ以上待たせない。
const updateNoticeTimeout = 2 * time.Second

// noticeIsTerminal は端末判定の差し替え点である。production では tui.IsTerminal を使う。
var noticeIsTerminal = tui.IsTerminal

// announceUpdate は新しいリリースがあることを stderr へ知らせる。daemon が持つ確認結果を読むだけなので、
// 起動がネットワーク待ちで遅くなることはない。案内はその版について 1 回だけで、案内権は daemon が渡す。
// 開発ビルドでは版を比較できないため、stderr が端末でないときは出力を汚さないため、いずれも何もしない。
func (c Client) announceUpdate(ctx context.Context) {
	if !update.ReleaseBuild() || !noticeIsTerminal(int(os.Stderr.Fd())) {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, updateNoticeTimeout)
	defer cancel()
	var status daemon.UpdateStatus
	// daemon が古くて method を知らない場合も、縮退で失敗する場合も、更新情報なしとして静かに扱う。
	if err := c.RPC.Call(callCtx, "UpdateStatus", rpc.UpdateStatusParams{ClaimAnnouncement: true}, &status); err != nil {
		return
	}
	for _, line := range updateNoticeLines(cliLocalizer(c), update.CurrentVersion(), status) {
		fmt.Fprintln(os.Stderr, line)
	}
}

// updateNoticeLines は案内として出す行を返す。案内しないときは nil を返す。
// daemon は自分の版で比べているため、呼び出し側の版でも比べ直してから出す。
func updateNoticeLines(localizer *i18n.Localizer, current string, status daemon.UpdateStatus) []string {
	if !status.Announce || !update.Newer(current, status.LatestVersion) {
		return nil
	}
	url := status.ReleaseURL
	if url == "" {
		url = update.ReleasesPage
	}
	notice := localizer.Localize("wx.update.notice", map[string]any{"Current": current, "Latest": status.LatestVersion})
	return []string{notice, url}
}
