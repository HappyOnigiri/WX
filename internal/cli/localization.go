package cli

import (
	"fmt"
	"os"

	"github.com/HappyOnigiri/WorktreeX/internal/daemon"
	"github.com/HappyOnigiri/WorktreeX/internal/i18n"
)

// localization.go は CLI の表示言語を 1 か所で決める。訳文は internal/i18n の
// カタログだけが持ち、ここは message ID と error を表示直前に解決する。

func cliLanguage(c Client) i18n.Language { return i18n.Normalize(c.Config.DisplayLanguage()) }

// cliLocalizer は 1 コマンド分の描画で使い回す resolver を返す。
// i18n.New は呼ぶたびに bundle を組み直すため、行ごとには呼ばない。
func cliLocalizer(c Client) *i18n.Localizer { return i18n.New(string(cliLanguage(c))) }

func cliError(c Client, err error) {
	reportErrorLanguage(cliLanguage(c), err)
}

// reportErrorLanguage は message ID を持つ error だけを訳し、外部 error は原文を出す。
func reportErrorLanguage(lang i18n.Language, err error) {
	localizer := i18n.New(string(lang))
	fmt.Fprintln(os.Stderr, localizer.Localize("cli.error_prefix", nil), leaseErrorText(localizer, err))
}

// leaseErrorText は daemon が返した worktree 無効エラーも表示時に訳す。
// RPC を越えると message ID が失われるため、marker 付きの固定文からだけ workspace root を取り出す。
func leaseErrorText(localizer *i18n.Localizer, err error) string {
	if root, ok := daemon.WorktreeDisabledRoot(err); ok {
		return localizer.Localize("cli.worktree_disabled", map[string]any{
			"Root": root, "Marker": daemon.WorktreeDisabledMarker,
		})
	}
	return localizer.Error(err)
}

// reportStepError は失敗した処理の名前を訳し、その原因は外部 error として原文で添える。
func reportStepError(lang i18n.Language, stepID string, err error) {
	localizer := i18n.New(string(lang))
	fmt.Fprintln(os.Stderr, localizer.Localize("cli.error_prefix", nil),
		localizer.Localize("cli.step.failed", map[string]any{
			"Step": localizer.Localize(stepID, nil), "Error": err.Error(),
		}))
}

func reportLeaseErrorLanguage(err error, lang i18n.Language) int {
	reportErrorLanguage(lang, err)
	if daemon.IsWorktreeDisabled(err) {
		return 2
	}
	return 1
}
