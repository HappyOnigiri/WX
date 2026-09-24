package main

import (
	"context"
	"testing"

	"github.com/HappyOnigiri/WorktreeX/internal/i18n"
)

// commandContext は呼び出し元が言語を決めている場合それを尊重し、無い場合だけ設定を読む。
func TestCommandContextKeepsExistingLanguage(t *testing.T) {
	ctx := commandContext(i18n.WithLanguage(context.Background(), string(i18n.Japanese)))
	if got := i18n.LanguageFromContext(ctx); got != i18n.Japanese {
		t.Fatalf("language=%q", got)
	}
	if got := commandContext(context.Background()); !i18n.HasLanguage(got) {
		t.Fatal("commandContext left the context without a language")
	}
}
