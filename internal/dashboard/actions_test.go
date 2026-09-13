package dashboard

import (
	"testing"

	"github.com/HappyOnigiri/WX/internal/i18n"
)

func TestActionsCoverEveryInteractiveTab(t *testing.T) {
	if len(tabIDs) != 6 {
		t.Fatalf("tabs=%d, want 6", len(tabIDs))
	}
	want := map[string]bool{
		"claude": false, "codex": false, "resume": false, "shell": false, "run": false, "new": false,
		"doctor": false, "gc": false, "clear": false, "prune": false, "retry-standby": false,
		"release": false, "discard-recovery": false, "forget": false, "bench": false, "daemon": false,
	}
	for tab, items := range tabMenus {
		if tab == 0 || tab == 2 || len(items) == 0 {
			t.Fatalf("invalid menu tab %d: %+v", tab, items)
		}
		for _, item := range items {
			if item.labelID == "" || item.descriptionID == "" || item.impactID == "" || item.command == "" {
				t.Errorf("incomplete item: %+v", item)
			}
			if _, ok := want[item.command]; ok {
				want[item.command] = true
			}
		}
	}
	for command, found := range want {
		if !found {
			t.Errorf("command %q is not reachable", command)
		}
	}
}

// TestMenuMessagesResolve は、catalog に無い ID を指したメニューが message ID
// そのものを画面へ出す退行を防ぐ。Localize は未知 ID に ID を返す。
func TestMenuMessagesResolve(t *testing.T) {
	catalog := i18n.Catalog()
	check := func(ids ...string) {
		t.Helper()
		for _, id := range ids {
			if id == "" {
				continue
			}
			if _, ok := catalog[id]; !ok {
				t.Errorf("message %q is not in the catalog", id)
			}
		}
	}
	check(tabIDs...)
	// 状態タブの更新項目は tabMenus に無いため、個別に照合しないと ID の欠落を見逃す。
	check(updateMenuItem.labelID, updateMenuItem.descriptionID, updateMenuItem.impactID)
	localizer := i18n.New(string(i18n.Japanese))
	for _, items := range tabMenus {
		for _, item := range items {
			check(item.labelID, item.descriptionID, item.impactID, item.inputLabelID)
			if item.argumentChoices == nil {
				continue
			}
			for _, option := range item.argumentChoices(localizer) {
				if _, ok := catalog[option.label]; ok || option.label == "" {
					t.Errorf("choice label %q is not localized", option.label)
				}
			}
		}
	}
}
