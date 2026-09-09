package main

import "testing"

func TestBenchCommandValidatesArgumentsAndHelp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	assertLeaseCommandHelp(t, "bench", runBench)
	assertLeaseCommandExits(t, "bench", runBench, []string{"extra"}, 2)
	assertLeaseCommandExits(t, "bench", runBench, []string{"--all"}, 2)
	// 回数の指定誤りは daemon へ接続する前に引数エラーで終える。
	assertLeaseCommandExits(t, "bench", runBench, []string{"--runs", "0"}, 2)
	// daemon が居なければ失敗（1）で終える。
	assertLeaseCommandExits(t, "bench", runBench, []string{"--reuse"}, 1)
}
