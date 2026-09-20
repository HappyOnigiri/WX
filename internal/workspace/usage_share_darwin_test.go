//go:build darwin

package workspace

import (
	"os"
	"strings"
	"testing"
)

func TestCompareCOWOffsetsChecksTheLastByte(t *testing.T) {
	t.Parallel()
	_, mainPath, _ := usageRoots(t)
	const size = 2 * 4096
	usageWrite(t, mainPath, "boundary", strings.Repeat("a", size))
	sourceFile, err := os.Open(mainPath + "/boundary")
	if err != nil {
		t.Fatal(err)
	}
	defer sourceFile.Close()
	targetFile, err := os.Open(mainPath + "/boundary")
	if err != nil {
		t.Fatal(err)
	}
	defer targetFile.Close()
	shared, comparable := compareCOWOffsets(sourceFile, targetFile, size)
	if !shared || !comparable {
		t.Fatalf("compareCOWOffsets()=(%t,%t), want shared and comparable", shared, comparable)
	}
}
