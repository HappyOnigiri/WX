package dashboard

import "testing"

func TestActionsCoverEveryInteractiveTab(t *testing.T) {
	if len(tabNames) != 6 {
		t.Fatalf("tabs=%d, want 6", len(tabNames))
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
			if item.label == "" || item.description == "" || item.impact == "" || item.command == "" {
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
