package main

import "testing"

func TestReleaseCommandValidatesArgumentsAndHelp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	assertLeaseCommandHelp(t, "release", runRelease)
	assertLeaseCommandExits(t, "release", runRelease, nil, 2)
	assertLeaseCommandExits(t, "release", runRelease, []string{"one", "two"}, 2)
	assertLeaseCommandExits(t, "release", runRelease, []string{"--json", "session"}, 2)
	// daemon が居なければ失敗（1）で終える。
	assertLeaseCommandExits(t, "release", runRelease, []string{"session"}, 1)
}
