package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	xansi "github.com/charmbracelet/x/ansi"

	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/i18n"
)

func cliLanguage(c Client) i18n.Language { return i18n.Normalize(c.Config.DisplayLanguage()) }

// cliLocalizer は表示言語を 1 度だけ解決する。i18n.T と i18n.New は
// 呼ぶたびに go-i18n の bundle を組み直すため、行ごとには呼ばない。
func cliLocalizer(c Client) *i18n.Localizer { return i18n.New(c.Config.DisplayLanguage()) }

// cliErrorPrefix は stderr の行頭に置く「error:」相当である。
func cliErrorPrefix(loc *i18n.Localizer) string { return loc.Localize("common.error", nil) + ":" }

// localizedError は wx 自身が作った固定文のエラーである。
// Error は英語を返して機械経路と wrap 済みの文脈を保ち、表示は message ID を利用者の言語で解決する。
type localizedError struct {
	id   string
	data map[string]any
	err  error
}

func (e *localizedError) Error() string {
	return i18n.New(string(i18n.English)).Localize(e.id, e.data)
}

// Unwrap は元の失敗を残す。errors.Is での判定経路をこの型が断ち切らないようにする。
func (e *localizedError) Unwrap() error { return e.err }

// newLocalizedError は固定文のエラーを作る。cause は表示に使わず、判定のために保持する。
func newLocalizedError(id string, data map[string]any, cause error) error {
	return &localizedError{id: id, data: data, err: cause}
}

// localizeCLIError は wx が作った固定文のエラーだけを訳す。
// daemon や外部コマンドから来たエラー本文は、訳語が値の一部に一致して壊れないよう原文のまま返す。
func localizeCLIError(loc *i18n.Localizer, err error) string {
	var localized *localizedError
	if errors.As(err, &localized) {
		return loc.Localize(localized.id, localized.data)
	}
	return err.Error()
}

func cliError(c Client, err error) {
	loc := cliLocalizer(c)
	fmt.Fprintln(os.Stderr, cliErrorPrefix(loc), localizeCLIError(loc, err))
}

func reportLeaseErrorLocalized(loc *i18n.Localizer, err error) int {
	fmt.Fprintln(os.Stderr, cliErrorPrefix(loc), localizeCLIError(loc, err))
	if daemon.IsWorktreeDisabled(err) {
		return 2
	}
	return 1
}

// padDisplay は表示幅で桁を揃える。訳語に空白を埋め込むと訳を変えた瞬間に桁がずれるため、
// 幅はここで測ってから連結する。left が真なら右寄せにする。
func padDisplay(value string, width int, left bool) string {
	pad := width - xansi.StringWidth(value)
	if pad <= 0 {
		return value
	}
	if left {
		return strings.Repeat(" ", pad) + value
	}
	return value + strings.Repeat(" ", pad)
}
