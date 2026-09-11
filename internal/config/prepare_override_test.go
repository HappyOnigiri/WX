package config

import (
	"strings"
	"testing"
)

// 指定した key だけが実効値を置き換え、残りは受け取った設定のままになる。
func TestPrepareOverrideAppliesOnlyTheSpecifiedKeys(t *testing.T) {
	t.Parallel()
	base := Defaults()
	base.Storage.CopyMode = CopyModeCOW
	base.Storage.COWMinSizeKiB = 32
	applied := PrepareOverride{CopyMode: CopyModeCopy}.Apply(base)
	if applied.CopyMode("/repo") != CopyModeCopy || applied.COWMinSizeKiB("/repo") != 32 {
		t.Fatalf("applied copy_mode=%s cow_min_size_kib=%d, want only the copy mode replaced", applied.CopyMode("/repo"), applied.COWMinSizeKiB("/repo"))
	}
	// 受け取った設定は変更しない。実効設定を差し替えると並走する準備まで巻き込む。
	if base.CopyMode("/repo") != CopyModeCOW {
		t.Fatalf("base=%+v, want the received configuration left untouched", base.Storage)
	}
	zero := 0
	lowered := PrepareOverride{COWMinSizeKiB: &zero}.Apply(base)
	if lowered.COWMinSizeKiB("/repo") != 0 || lowered.CopyMode("/repo") != CopyModeCOW {
		t.Fatalf("lowered copy_mode=%s cow_min_size_kib=%d, want the zero lower bound applied", lowered.CopyMode("/repo"), lowered.COWMinSizeKiB("/repo"))
	}
}

// 貸出1回の上書きは repository 個別指定より優先される。
// Storage を書き換える実装では個別指定が上に残り、その repository でだけ上書きが黙って無効になった。
func TestPrepareOverrideWinsOverRepositoryOverride(t *testing.T) {
	t.Parallel()
	base := Defaults()
	pinned := 64
	base.Repositories["/repo"] = Repository{COWMinSizeKiB: &pinned, Storage: RepositoryStorage{CopyMode: CopyModeCOW}}
	zero := 0
	applied := PrepareOverride{CopyMode: CopyModeCopy, COWMinSizeKiB: &zero}.Apply(base)
	if applied.CopyMode("/repo") != CopyModeCopy || applied.COWMinSizeKiB("/repo") != 0 {
		t.Fatalf("copy_mode=%s cow_min_size_kib=%d, want the lease override to win", applied.CopyMode("/repo"), applied.COWMinSizeKiB("/repo"))
	}
	// 上書きの無い repository は個別指定のまま残る。
	if applied.CopyMode("/other") != CopyModeCopy {
		t.Fatalf("copy_mode=%s for an unlisted repository, want the lease override", applied.CopyMode("/other"))
	}
	if base.CopyMode("/repo") != CopyModeCOW || base.COWMinSizeKiB("/repo") != 64 {
		t.Fatalf("base copy_mode=%s cow_min_size_kib=%d, want the repository override untouched", base.CopyMode("/repo"), base.COWMinSizeKiB("/repo"))
	}
}

// 上書きなしは Apply でも Encode でも何も変えない。
func TestPrepareOverrideZeroValueChangesNothing(t *testing.T) {
	t.Parallel()
	var override PrepareOverride
	if !override.IsZero() || override.String() != "" {
		t.Fatalf("override=%+v label=%q, want an empty override", override, override.String())
	}
	encoded, err := override.Encode()
	if err != nil || encoded != "" {
		t.Fatalf("encoded=%q err=%v, want an empty record", encoded, err)
	}
	decoded, err := DecodePrepareOverride("")
	if err != nil || !decoded.IsZero() {
		t.Fatalf("decoded=%+v err=%v, want an empty override", decoded, err)
	}
}

// 記録と復元は往復する。0 の下限も未指定と混ざらない。
func TestPrepareOverrideRoundTripsThroughItsRecord(t *testing.T) {
	t.Parallel()
	zero := 0
	override := PrepareOverride{CopyMode: CopyModeCopy, COWMinSizeKiB: &zero}
	encoded, err := override.Encode()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodePrepareOverride(encoded)
	if err != nil || decoded.CopyMode != CopyModeCopy || decoded.COWMinSizeKiB == nil || *decoded.COWMinSizeKiB != 0 {
		t.Fatalf("decoded=%+v err=%v from %q", decoded, err, encoded)
	}
	if _, err := DecodePrepareOverride(`{"copy_mode":"clone"}`); err == nil {
		t.Fatal("want an invalid recorded copy mode rejected")
	}
}

// 指定の解釈は storage の同名設定と同じ条件で値を検査する。
func TestParsePrepareOverrideAcceptsAndRejects(t *testing.T) {
	t.Parallel()
	parsed, err := ParsePrepareOverride("copy_mode=copy,cow_min_size_kib=64")
	if err != nil || parsed.CopyMode != CopyModeCopy || parsed.COWMinSizeKiB == nil || *parsed.COWMinSizeKiB != 64 {
		t.Fatalf("parsed=%+v err=%v", parsed, err)
	}
	if parsed.String() != "copy_mode=copy,cow_min_size_kib=64" {
		t.Fatalf("label=%q, want the keys in a stable order", parsed.String())
	}
	for _, spec := range []string{"", "copy_mode", "copy_mode=clone", "cow_min_size_kib=-1", "cow_min_size_kib=x", "warm=1", "copy_mode=cow,copy_mode=copy"} {
		if _, err := ParsePrepareOverride(spec); err == nil {
			t.Fatalf("spec=%q, want it rejected", spec)
		}
	}
	if _, err := ParsePrepareOverride("cow_min_size_kib=" + strings.Repeat("9", 12)); err == nil {
		t.Fatal("want a lower bound above the maximum rejected")
	}
}
