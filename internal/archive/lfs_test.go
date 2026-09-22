package archive

import (
	"bytes"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/workspace"
)

func TestSnapshotTreeDiffCursorProgress(t *testing.T) {
	tests := []struct {
		name     string
		previous int
		current  int
		wantErr  bool
	}{
		{name: "retreated", previous: 1, current: 0, wantErr: true},
		{name: "unchanged", previous: 1, current: 1, wantErr: true},
		{name: "advanced", previous: 1, current: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSnapshotTreeDiffProgress(tt.previous, tt.current)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateSnapshotTreeDiffProgress(%d, %d) error=%v, wantErr=%v", tt.previous, tt.current, err, tt.wantErr)
			}
		})
	}
}

func TestParseSnapshotTreeDiffSelectsNewRegularBlobs(t *testing.T) {
	old := strings.Repeat("1", 40)
	newOID := strings.Repeat("2", 40)
	deleted := strings.Repeat("0", 40)
	output := ":100644 100644 " + old + " " + newOID + " M\x00weights.bin\x00" +
		":100644 000000 " + old + " " + deleted + " D\x00removed.bin\x00" +
		":000000 100644 " + deleted + " " + newOID + " A\x00added.bin\x00" +
		":100644 100644 " + old + " " + newOID + " M\x00regular.txt\x00" +
		":100644 100644 " + old + " " + newOID + " M\x00../unsafe\x00"
	changes, err := parseSnapshotTreeDiff(output)
	if err == nil || len(changes) != 0 {
		t.Fatalf("unsafe tree path was not rejected: changes=%+v err=%v", changes, err)
	}
	output = ":100644 100644 " + old + " " + newOID + " M\x00weights.bin\x00" +
		":100644 000000 " + old + " " + deleted + " D\x00removed.bin\x00" +
		":000000 100644 " + deleted + " " + newOID + " A\x00added.bin\x00" +
		":160000 160000 " + old + " " + newOID + " M\x00module\x00"
	changes, err = parseSnapshotTreeDiff(output)
	if err != nil || len(changes) != 2 || changes[0].path != "weights.bin" || changes[0].newOID != newOID || changes[1].path != "added.bin" {
		t.Fatalf("changes=%+v err=%v", changes, err)
	}
}

func TestParseSnapshotTreeDiffRejectsHeaderWithoutPath(t *testing.T) {
	old := strings.Repeat("1", 40)
	newOID := strings.Repeat("2", 40)
	output := ":100644 100644 " + old + " " + newOID + " M"
	if _, err := parseSnapshotTreeDiff(output); err == nil || !strings.Contains(err.Error(), "invalid Git tree diff record") {
		t.Fatalf("header without path error=%v", err)
	}
}

func TestParseSnapshotLFSPointerBatch(t *testing.T) {
	oid := strings.Repeat("a", 40)
	pointerData := "version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat("b", 64) + "\nsize 42\n"
	// cat-file --batch headers carry decimal byte counts, not runes.
	output := oid + " blob " + strconv.Itoa(len(pointerData)) + "\n" + pointerData + "\n"
	pointers, err := parseSnapshotLFSPointerBatch(output, []string{oid})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := pointers[oid]
	if !ok || got.OID != "sha256:"+strings.Repeat("b", 64) || got.Size != 42 {
		t.Fatalf("pointer=%+v ok=%v", got, ok)
	}
}

func TestParseSnapshotLFSPointerBatchAcceptsZeroByteBlob(t *testing.T) {
	oid := strings.Repeat("a", 40)
	output := oid + " blob 0\n\n"
	pointers, err := parseSnapshotLFSPointerBatch(output, []string{oid})
	if err != nil {
		t.Fatalf("zero-byte blob returned an error: %v", err)
	}
	if len(pointers) != 0 {
		t.Fatalf("zero-byte non-pointer blob produced pointers=%v", pointers)
	}
}

func TestParseSnapshotLFSPointerBatchSkipsMissingBlob(t *testing.T) {
	oid := strings.Repeat("a", 40)
	pointers, err := parseSnapshotLFSPointerBatch(oid+" missing\n", []string{oid})
	if err != nil {
		t.Fatalf("missing blob returned an error: %v", err)
	}
	if len(pointers) != 0 {
		t.Fatalf("missing blob produced pointers=%v", pointers)
	}
}

func TestParseSnapshotLFSPointerBatchAcceptsMaximumSizedPointerBlob(t *testing.T) {
	oid := strings.Repeat("a", 40)
	pointerData := "version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat("b", 64) + "\nsize 42\n"
	pointerData += strings.Repeat("x", maxLFSPointerBlobBytes-len(pointerData))
	output := oid + " blob " + strconv.Itoa(len(pointerData)) + "\n" + pointerData + "\n"
	pointers, err := parseSnapshotLFSPointerBatch(output, []string{oid})
	if err != nil {
		t.Fatalf("maximum-sized pointer blob returned an error: %v", err)
	}
	got, ok := pointers[oid]
	if !ok || got.OID != "sha256:"+strings.Repeat("b", 64) || got.Size != 42 {
		t.Fatalf("pointer=%+v ok=%v", got, ok)
	}
}

func TestLogLFSOptimizationWarning(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	manager := &Manager{Preparer: &workspace.Preparer{Log: slog.New(slog.NewTextHandler(&logged, nil))}}
	manager.logLFSOptimizationWarning("test", errors.New("test failure"))
	if !strings.Contains(logged.String(), "LFS cache optimization skipped") {
		t.Fatalf("log output=%q", logged.String())
	}
}
