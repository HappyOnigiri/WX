package archive

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WorktreeX/internal/domain"
)

// TestMutationWorkspaceArchiveEntryKinds は workspace archive の tar entry が
// directory、空/非空 regular file、symlink の種別と payload を保つことを確認する。
func TestMutationWorkspaceArchiveEntryKinds(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "empty"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "nonempty"), []byte("payload\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nonempty", filepath.Join(rootPath, "link")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	entries, err := os.ReadDir(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for _, entry := range entries {
		if err := writeWorkspaceArchiveEntry(root, writer, entry.Name(), entry); err != nil {
			t.Fatalf("write %s: %v", entry.Name(), err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	want := map[string]struct {
		typeflag byte
		size     int64
		linkname string
		content  string
	}{
		"directory": {typeflag: tar.TypeDir},
		"empty":     {typeflag: tar.TypeReg, size: 0},
		"nonempty":  {typeflag: tar.TypeReg, size: int64(len("payload\n")), content: "payload\n"},
		"link":      {typeflag: tar.TypeSymlink, linkname: "nonempty"},
	}
	reader := tar.NewReader(bytes.NewReader(archive.Bytes()))
	for {
		header, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		test, ok := want[header.Name]
		if !ok {
			t.Fatalf("unexpected archive entry %q", header.Name)
		}
		if header.Typeflag != test.typeflag || header.Size != test.size || header.Linkname != test.linkname {
			t.Fatalf("entry %q type=%d size=%d link=%q want type=%d size=%d link=%q", header.Name, header.Typeflag, header.Size, header.Linkname, test.typeflag, test.size, test.linkname)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != test.content {
			t.Fatalf("entry %q body=%q want %q", header.Name, body, test.content)
		}
		delete(want, header.Name)
	}
	if len(want) != 0 {
		t.Fatalf("archive omitted entries: %v", want)
	}
}

// TestMutationWorkspaceRestoreEntryBoundaries は zero-byte file、短い payload、
// directory collision、symlink collision、unsupported tar type を実体で検証する。
func TestMutationWorkspaceRestoreEntryBoundaries(t *testing.T) {
	rootPath := t.TempDir()
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	for _, test := range []struct {
		name    string
		header  tar.Header
		payload string
		wantErr string
	}{
		{name: "empty regular", header: tar.Header{Name: "empty", Typeflag: tar.TypeReg, Mode: 0o640}},
		{name: "nonempty regular", header: tar.Header{Name: "nonempty", Typeflag: tar.TypeReg, Mode: 0o640, Size: 8}, payload: "payload\n"},
		{name: "short regular", header: tar.Header{Name: "short", Typeflag: tar.TypeReg, Mode: 0o600, Size: 8}, payload: "short", wantErr: "EOF"},
		{name: "unsupported", header: tar.Header{Name: "hardlink", Typeflag: tar.TypeLink, Linkname: "target"}, wantErr: "unsupported tar type"},
	} {
		t.Run(test.name, func(t *testing.T) {
			seen := map[string]byte{}
			var reader *tar.Reader
			var err error
			if test.name == "short regular" {
				reader = tar.NewReader(strings.NewReader(test.payload))
				err = restoreWorkspaceRegularFile(root, reader, filepath.FromSlash(test.header.Name), test.header.Name, &test.header)
			} else {
				reader = mutationTarReader(t, test.header, test.payload)
				err = restoreWorkspaceEntry(root, reader, &test.header, nil, seen)
			}
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error=%v want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			data, readErr := os.ReadFile(filepath.Join(rootPath, test.header.Name))
			if readErr != nil || string(data) != test.payload {
				t.Fatalf("restored %q=%q err=%v", test.header.Name, data, readErr)
			}
		})
	}

	if err := os.WriteFile(filepath.Join(rootPath, "link-collision"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	header := tar.Header{Name: "link-collision", Typeflag: tar.TypeSymlink, Linkname: "elsewhere"}
	reader := mutationTarReader(t, header, "")
	if err := restoreWorkspaceEntry(root, reader, &header, nil, map[string]byte{}); err == nil {
		t.Fatal("symlink collision was accepted")
	}
	if data, err := os.ReadFile(filepath.Join(rootPath, "link-collision")); err != nil || string(data) != "keep" {
		t.Fatalf("symlink collision changed existing file=%q err=%v", data, err)
	}

	closed, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	directoryHeader := tar.Header{Name: "closed-root", Typeflag: tar.TypeDir, Mode: 0o700}
	if err := restoreWorkspaceEntry(closed, mutationTarReader(t, directoryHeader, ""), &directoryHeader, nil, map[string]byte{}); err == nil {
		t.Fatal("closed root accepted directory restore")
	}

	if err := os.Chmod(rootPath, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(rootPath, 0o700) })
	permissionHeader := tar.Header{Name: "permission-denied", Typeflag: tar.TypeDir, Mode: 0o700}
	if err := restoreWorkspaceEntry(root, mutationTarReader(t, permissionHeader, ""), &permissionHeader, nil, map[string]byte{}); err == nil {
		t.Fatal("directory creation failure was accepted")
	}
}

type mutationErrorReader struct {
	data   []byte
	failAt int
	offset int
	err    error
}

func (reader *mutationErrorReader) Read(p []byte) (int, error) {
	if reader.offset >= reader.failAt {
		return 0, reader.err
	}
	n := len(p)
	if remaining := reader.failAt - reader.offset; n > remaining {
		n = remaining
	}
	copy(p[:n], reader.data[reader.offset:reader.offset+n])
	reader.offset += n
	return n, nil
}

func TestMutationWorkspaceRestoreRegularFilePreservesReaderError(t *testing.T) {
	rootPath := t.TempDir()
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	const payload = "payload"
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	header := tar.Header{Name: "payload", Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(payload))}
	if err := writer.WriteHeader(&header); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("archive stream interrupted")
	stream := &mutationErrorReader{data: archive.Bytes(), failAt: 512 + len(payload) - 1, err: wantErr}
	reader := tar.NewReader(stream)
	if _, err := reader.Next(); err != nil {
		t.Fatal(err)
	}
	if err := restoreWorkspaceRegularFile(root, reader, "payload", "payload", &header); !errors.Is(err, wantErr) {
		t.Fatalf("restore error=%v want %v", err, wantErr)
	}
}

// TestMutationWorkspaceArchiveRejectsReplacement は WalkDir の entry を取得した後に
// 実体が別 inode へ置き換わった場合、別内容を archive へ混ぜないことを確認する。
func TestMutationWorkspaceArchiveRejectsReplacement(t *testing.T) {
	rootPath := t.TempDir()
	filePath := filepath.Join(rootPath, "changed")
	if err := os.WriteFile(filePath, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	oldInfo, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filePath, filePath+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filePath, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := writeWorkspaceArchiveEntry(root, tar.NewWriter(&archive), "changed", mutationStaticDirEntry{info: oldInfo}); err == nil || !strings.Contains(err.Error(), "changed while snapshotting") {
		t.Fatalf("replacement was accepted: %v", err)
	}
}

// TestMutationWorkspaceArchivePropagatesCopyFailure は tar header の書込み後に
// payload 書込みが失敗した場合、close 成功へすり替えないことを確認する。
func TestMutationWorkspaceArchivePropagatesCopyFailure(t *testing.T) {
	rootPath := t.TempDir()
	filePath := filepath.Join(rootPath, "payload")
	if err := os.WriteFile(filePath, []byte("payload\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	entries, err := os.ReadDir(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(&mutationFailAfterHeaderWriter{remaining: 512, err: errors.New("payload write failed")})
	err = writeWorkspaceArchiveEntry(root, writer, "payload", entries[0])
	if err == nil || !strings.Contains(err.Error(), "payload write failed") {
		t.Fatalf("payload write failure was swallowed: %v", err)
	}
}

type mutationFailAfterHeaderWriter struct {
	remaining int
	err       error
}

func (w *mutationFailAfterHeaderWriter) Write(p []byte) (int, error) {
	if w.remaining == 0 {
		return 0, w.err
	}
	n := len(p)
	if n > w.remaining {
		n = w.remaining
	}
	w.remaining -= n
	return n, nil
}

var _ io.Writer = (*mutationFailAfterHeaderWriter)(nil)

type mutationStaticDirEntry struct{ info fs.FileInfo }

func (entry mutationStaticDirEntry) Name() string      { return entry.info.Name() }
func (entry mutationStaticDirEntry) IsDir() bool       { return entry.info.IsDir() }
func (entry mutationStaticDirEntry) Type() fs.FileMode { return entry.info.Mode().Type() }
func (entry mutationStaticDirEntry) Info() (fs.FileInfo, error) {
	return entry.info, nil
}

func mutationTarReader(t *testing.T, header tar.Header, payload string) *tar.Reader {
	t.Helper()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if header.Size == 0 && payload != "" {
		header.Size = int64(len(payload))
	}
	if err := writer.WriteHeader(&header); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(bytes.NewReader(archive.Bytes()))
	if _, err := reader.Next(); err != nil {
		t.Fatal(err)
	}
	return reader
}

// TestMutationWorkspaceRestoreVerifiedWorkspaceRejectsClosedArchive は検証済み
// descriptor を閉じた状態で prune/restore を始めないことを確認する。
func TestMutationWorkspaceRestoreVerifiedWorkspaceRejectsClosedArchive(t *testing.T) {
	ownershipRoot := t.TempDir()
	bundleRoot := filepath.Join(ownershipRoot, "bundle")
	if err := os.Mkdir(bundleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(ownershipRoot, ownershipRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	snapshot, err := SnapshotWorkspaceAt(context.Background(), bundleRoot, ownershipRoot, testRootID, owner, "closed-verified", nil, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	verified, err := OpenVerifiedWorkspaceSnapshotAt(context.Background(), ownershipRoot, owner, snapshot, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := verified.Close(); err != nil {
		t.Fatal(err)
	}
	if err := RestoreVerifiedWorkspace(context.Background(), verified, bundleRoot, ownershipRoot, owner, nil); err == nil {
		t.Fatal("closed archive descriptor was accepted")
	}
}

func TestMutationDeleteWorkspaceSnapshotChecksRootAfterRemoval(t *testing.T) {
	ownershipRoot := t.TempDir()
	bundleRoot := filepath.Join(ownershipRoot, "bundle")
	if err := os.Mkdir(bundleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(ownershipRoot, ownershipRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	snapshot, err := SnapshotWorkspaceAt(context.Background(), bundleRoot, ownershipRoot, testRootID, owner, "delete-root-check", nil, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("root changed after archive removal")
	verifyCalls := 0
	verifyRoot := func(string, *os.Root) error {
		verifyCalls++
		if verifyCalls == 2 {
			return wantErr
		}
		return nil
	}
	if err := deleteWorkspaceSnapshotAt(context.Background(), ownershipRoot, owner, snapshot, verifyRoot, openWorkspaceSnapshotDirectory, syncWorkspaceSnapshotDirectory); !errors.Is(err, wantErr) {
		t.Fatalf("delete error=%v want %v", err, wantErr)
	}
}

func TestMutationDeleteWorkspaceSnapshotPropagatesDirectorySyncFailure(t *testing.T) {
	ownershipRoot := t.TempDir()
	bundleRoot := filepath.Join(ownershipRoot, "bundle")
	if err := os.Mkdir(bundleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(ownershipRoot, ownershipRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	snapshot, err := SnapshotWorkspaceAt(context.Background(), bundleRoot, ownershipRoot, testRootID, owner, "delete-sync-check", nil, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("workspace snapshot directory sync failed")
	openDirectory := func(*os.Root) (*os.File, error) {
		return os.Open(os.DevNull)
	}
	syncDirectory := func(*os.File) error { return wantErr }
	if err := deleteWorkspaceSnapshotAt(context.Background(), ownershipRoot, owner, snapshot, verifyPinnedRootPath, openDirectory, syncDirectory); !errors.Is(err, wantErr) {
		t.Fatalf("delete error=%v want %v", err, wantErr)
	}
}
