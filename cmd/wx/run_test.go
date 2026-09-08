package main

import "testing"

func TestRunCommandValidatesArgumentsAndHelp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	assertLeaseCommandHelp(t, "run", runRun)
	assertLeaseCommandExits(t, "run", runRun, nil, 2)
	// コマンドの argv は先頭の wx オプションだけを解釈するため、未知の wx オプションは誤用として断る。
	assertLeaseCommandExits(t, "run", runRun, []string{"--worktree", "--", "true"}, 2)
	assertLeaseCommandExits(t, "run", runRun, []string{"--branch", "main", "--resume", "session", "--", "true"}, 2)
}
