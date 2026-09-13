package daemon

import "testing"

// 奇数個の pairs は最後の名前を捨て、残りをそのまま template のフィールドにする。
func TestMessageBuildsDataFromPairs(t *testing.T) {
	t.Parallel()
	if plain := message("diag.label.cause"); plain.ID != "diag.label.cause" || plain.Data != nil {
		t.Fatalf("message without pairs=%+v", plain)
	}
	built := message("diag.detail.job", "JobID", "job-1", "Dropped")
	if built.Data["JobID"] != "job-1" || len(built.Data) != 1 {
		t.Fatalf("message data=%+v", built.Data)
	}
	// 名前が string でない組は無視し、後続の組を落とさない。
	mixed := message("diag.detail.job", 1, "ignored", "JobID", "job-2")
	if mixed.Data["JobID"] != "job-2" || len(mixed.Data) != 1 {
		t.Fatalf("message data=%+v", mixed.Data)
	}
}
