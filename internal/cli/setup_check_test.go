package cli

import "testing"

func TestSetupCheckUsesDistinctAgentKind(t *testing.T) {
	if setupCheckAgentKind == "" || setupCheckAgentKind == probeAgentKind {
		t.Fatalf("setup check agent kind=%q", setupCheckAgentKind)
	}
}
