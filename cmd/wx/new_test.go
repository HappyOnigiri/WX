package main

import "testing"

func TestNewCommandValidatesArgumentsAndHelp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	assertLeaseCommandHelp(t, "new", runNew)
	assertLeaseCommandExits(t, "new", runNew, []string{"extra"}, 2)
	assertLeaseCommandExits(t, "new", runNew, []string{"--discard"}, 2)
	// daemon が居なければ失敗（1）で終える。
	assertLeaseCommandExits(t, "new", runNew, nil, 1)
}
