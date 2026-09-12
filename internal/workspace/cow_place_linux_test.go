package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/HappyOnigiri/WX/internal/config"
)

// CoW の無い platform では clone が必ず失敗するので、先行配置は1件も置かずに失敗を上げる。
// auto はその失敗を記録して通常 checkout へ委ね、strict な cow だけが準備を失敗させる。
func TestCOWPlacementReportsCloneFailureWhereCoWIsUnsupported(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, repo, preparer, item := stagedCOWFixture(t, map[string]string{"big/donor.bin": strings.Repeat("b", cowMinShareSize) + "\n"})
	placement, err := preparer.placeOwnedSharedFiles(ctx, repo, item, testSlotID)
	if !errors.Is(err, unix.ENOTSUP) {
		t.Fatalf("clone err=%v", err)
	}
	if len(placement.placed) != 0 {
		t.Fatalf("placed=%v", placement.placed)
	}
	if _, statErr := os.Stat(filepath.Join(item.Target, "big", "donor.bin")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("destination leaf err=%v", statErr)
	}
	if fallbackErr := preparer.cowFallback(ctx, config.CopyModeAuto, item.Target, err); fallbackErr != nil {
		t.Fatalf("auto fallback err=%v", fallbackErr)
	}
	if fallbackErr := preparer.cowFallback(ctx, config.CopyModeCOW, item.Target, err); !errors.Is(fallbackErr, unix.ENOTSUP) {
		t.Fatalf("strict CoW fallback err=%v", fallbackErr)
	}
}

// CoW の無い platform では共有経路へ入らないので、placeSharedFiles は1件も置かず失敗もしない。
func TestPlaceSharedFilesSkipsEntirelyWhereCoWIsUnsupported(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, repo, preparer, item := stagedCOWFixture(t, map[string]string{"big/donor.bin": strings.Repeat("b", cowMinShareSize) + "\n"})
	placement, err := preparer.placeSharedFiles(ctx, repo, item, testSlotID)
	if err != nil {
		t.Fatal(err)
	}
	if len(placement.placed) != 0 {
		t.Fatalf("placed=%v", placement.placed)
	}
}
