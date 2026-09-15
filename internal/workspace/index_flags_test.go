package workspace

import (
	"reflect"
	"testing"
)

func TestParseIndexFlagsSeparatesSkipWorktreeFromAssumeUnchanged(t *testing.T) {
	t.Parallel()
	listing := "H plain\x00S skipped\x00h assumed\x00s both\x00M unmerged\x00S skipped\x00\x00"
	flags := ParseIndexFlags(listing)
	if want := []string{"skipped", "both"}; !reflect.DeepEqual(flags.SkipWorktree, want) {
		t.Fatalf("skip-worktree=%v, want %v", flags.SkipWorktree, want)
	}
	if want := []string{"assumed", "both"}; !reflect.DeepEqual(flags.AssumeUnchanged, want) {
		t.Fatalf("assume-unchanged=%v, want %v", flags.AssumeUnchanged, want)
	}
	if !flags.Blinding() {
		t.Fatal("listing with stat flags was not reported as blinding")
	}
	if !flags.Has("both") || flags.Has("plain") {
		t.Fatalf("Has misreported the flagged paths: %+v", flags.FlaggedPaths)
	}
	if want := []string{"plain", "both"}; !reflect.DeepEqual(flags.Retain([]string{"plain", "gone", "both"}), want) {
		t.Fatalf("retain kept %v, want %v", flags.Retain([]string{"plain", "gone", "both"}), want)
	}
	if want := []string{"skipped"}; !reflect.DeepEqual(flags.Without([]string{"both"}).SkipWorktree, want) {
		t.Fatalf("Without kept %v, want %v", flags.Without([]string{"both"}).SkipWorktree, want)
	}
}

func TestParseIndexFlagsIgnoresEmptyListing(t *testing.T) {
	t.Parallel()
	flags := ParseIndexFlags("")
	if flags.Blinding() || len(flags.Paths) != 0 {
		t.Fatalf("empty listing produced %+v", flags)
	}
}

func TestNULPathListEncodesNULSeparatedEntries(t *testing.T) {
	t.Parallel()
	if got, want := string(NULPathList([]string{"a*.txt", "b"})), "a*.txt\x00b\x00"; got != want {
		t.Fatalf("NULPathList=%q, want %q", got, want)
	}
}
