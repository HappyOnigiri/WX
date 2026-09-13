package main

import (
	"context"
	"testing"

	"github.com/HappyOnigiri/WX/internal/i18n"
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

// applyTranslations は宣言順に適用する。後の要素が前の訳出を上書きできることで順序を確かめる。
func TestApplyTranslationsFollowsDeclarationOrder(t *testing.T) {
	ordered := []translation{{"ab", "X"}, {"a", "Y"}}
	if got := applyTranslations("aba", ordered); got != "XY" {
		t.Fatalf("ordered=%q", got)
	}
	reversed := []translation{{"a", "Y"}, {"ab", "X"}}
	if got := applyTranslations("aba", reversed); got != "YbY" {
		t.Fatalf("reversed=%q", got)
	}
}
