package identity

import "testing"

func TestSessionIdentity(t *testing.T) {
	if got := ComputeSessionStableID("claude", "11111111-1111-4111-8111-111111111111"); got != "s1-a7762609eeea225f" {
		t.Fatal(got)
	}
	if ComputeSessionStableID("claude", "") != "" {
		t.Fatal("empty ID received an identity")
	}
	if ComputeSessionStableID("claude", "x") == ComputeSessionStableID("codex", "x") {
		t.Fatal("agents must be distinct")
	}
}
