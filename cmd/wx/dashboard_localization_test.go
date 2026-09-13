package main

import (
	"context"
	"testing"

	"github.com/HappyOnigiri/WX/internal/i18n"
	"github.com/HappyOnigiri/WX/internal/setup"
)

// TestLocalizedSetupStepsTranslatesTitlesAndKeepsIdentifiers は dashboard へ渡す前に
// 見出しを解決することを検査する。列幅は解決済みラベルの実幅から決まるため、
// レイアウトより後で訳すと桁が合わない。
func TestLocalizedSetupStepsTranslatesTitlesAndKeepsIdentifiers(t *testing.T) {
	steps := []setup.Step{
		{ID: "prerequisites", Title: "Prerequisites", State: setup.StatePresent, Detail: "checked without changing anything"},
		{ID: "hooks.claude", Title: "Agent hooks (claude)", State: setup.StateDivergent},
	}
	got := localizedSetupSteps(i18n.WithLanguage(context.Background(), string(i18n.Japanese)), steps)
	if got[0].Title != "前提条件" || got[1].Title != "Agent hook (claude)" {
		t.Fatalf("titles=%q, %q", got[0].Title, got[1].Title)
	}
	// ID・state・detail は機械識別子と可変値なので置き換えない。
	if got[0].ID != "prerequisites" || got[0].State != setup.StatePresent || got[0].Detail != "checked without changing anything" {
		t.Fatalf("localization changed a machine value: %+v", got[0])
	}
	// 元の slice は変更しない。呼び出し側は RPC 応答をそのまま持ち回る。
	if steps[0].Title != "Prerequisites" {
		t.Fatalf("localization mutated the input=%q", steps[0].Title)
	}
	english := localizedSetupSteps(i18n.WithLanguage(context.Background(), string(i18n.English)), steps)
	if english[0].Title != "Prerequisites" {
		t.Fatalf("English title=%q", english[0].Title)
	}
	if localizedSetupSteps(context.Background(), nil) != nil {
		t.Fatal("localizedSetupSteps invented a slice for no steps")
	}
}
