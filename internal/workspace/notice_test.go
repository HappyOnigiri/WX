package workspace

import (
	"sync"
	"testing"
)

func TestPrepareNoticesRecordsOutputInOrder(t *testing.T) {
	notices := &PrepareNotices{}
	notices.Add(PrepareNotice{Target: "/slot/repo", Phase: "post-checkout", Stderr: "hook warned\n"})
	notices.Add(PrepareNotice{Target: "/slot/other", Phase: "post-checkout", Stdout: "hook said something\n"})
	got := notices.Notices()
	if len(got) != 2 {
		t.Fatalf("notices = %d entries, want 2", len(got))
	}
	if got[0].Target != "/slot/repo" || got[0].Stderr != "hook warned\n" {
		t.Fatalf("first notice = %+v", got[0])
	}
	if got[1].Stdout != "hook said something\n" {
		t.Fatalf("second notice = %+v", got[1])
	}
}

// 出力の無い呼び出しまで記録すると、区間を通っただけの回が診断へ並んでしまう。
func TestPrepareNoticesIgnoresEmptyOutput(t *testing.T) {
	notices := &PrepareNotices{}
	notices.Add(PrepareNotice{Target: "/slot/repo", Phase: "post-checkout"})
	if got := notices.Notices(); len(got) != 0 {
		t.Fatalf("notices = %+v, want none", got)
	}
}

// nil の器でも準備は同じ結果になり、記録だけが落ちる。
func TestPrepareNoticesNilIsUsable(t *testing.T) {
	var notices *PrepareNotices
	notices.Add(PrepareNotice{Target: "/slot/repo", Phase: "post-checkout", Stderr: "output"})
	if got := notices.Notices(); got != nil {
		t.Fatalf("notices = %+v, want nil", got)
	}
}

// 準備は repository ごとに並列で進むため、記録は同時呼び出しで壊れてはならない。
func TestPrepareNoticesAddsConcurrently(t *testing.T) {
	notices := &PrepareNotices{}
	var wait sync.WaitGroup
	for index := range 16 {
		wait.Go(func() {
			notices.Add(PrepareNotice{Target: "/slot/repo", Phase: "post-checkout", Stderr: string(rune('a' + index))})
		})
	}
	wait.Wait()
	if got := notices.Notices(); len(got) != 16 {
		t.Fatalf("notices = %d entries, want 16", len(got))
	}
}
