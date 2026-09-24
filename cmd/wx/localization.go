package main

import (
	"context"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
	"github.com/HappyOnigiri/WorktreeX/internal/i18n"
)

// localization.go は cmd/wx が表示言語を決める土台を持つ。
// 訳文は internal/i18n のカタログだけが持ち、描画側は message ID を引く。

// commandContext は直接サブコマンド関数を呼ぶテストと、run からの通常起動の
// どちらにも設定言語を明示する。壊れた設定は config.LoadLanguage の英語 fallback を使う。
func commandContext(ctx context.Context) context.Context {
	if i18n.HasLanguage(ctx) {
		return ctx
	}
	return i18n.WithLanguage(ctx, config.LoadLanguage())
}
