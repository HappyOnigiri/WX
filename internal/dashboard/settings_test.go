package dashboard

import (
	"context"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
)

func TestConfigEnvironmentsAreSortedAfterGlobal(t *testing.T) {
	cfg := config.Defaults()
	cfg.Workspaces["/tmp/zeta"] = config.Workspace{}
	cfg.Workspaces["/tmp/alpha"] = config.Workspace{}
	m := newModel(context.Background(), Options{Config: cfg})
	got := m.configEnvironments()
	if len(got) != 3 || got[0].label != "Global" || got[1].label != "alpha" || got[2].label != "zeta" {
		t.Fatalf("environments=%+v", got)
	}
}
