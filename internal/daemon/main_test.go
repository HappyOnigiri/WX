package daemon

import (
	"os"
	"testing"

	"github.com/HappyOnigiri/WX/internal/testutil"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.RunWithPhysicalTempDir(m))
}
