package archive

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestResolveBundleLinkExclusionsSelectsSymlinksOnly は snapshot 側の除外が実体だけで決まることを固定する。
// 実体のある候補まで除外すると、貸出中に link rule を足しただけで slot の作業が tar から落ちる。
func TestResolveBundleLinkExclusionsSelectsSymlinksOnly(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	bundleRoot := filepath.Join(base, "bundle")
	if err := os.MkdirAll(filepath.Join(bundleRoot, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceTestFile(t, filepath.Join(bundleRoot, "docs", "note.md"), "work\n", 0o600)
	source := filepath.Join(base, "source")
	if err := os.MkdirAll(filepath.Join(source, "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(source, filepath.Join(bundleRoot, "shared")); err != nil {
		t.Fatal(err)
	}
	bundle, err := os.OpenRoot(bundleRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bundle.Close() }()

	// "shared/inner" は祖先が除外済みなので lstat されない。"missing" は不存在、"../escape" と "" は不正な候補である。
	got, err := resolveBundleLinkExclusions(bundle, []string{"shared/inner", "docs", "shared", "missing", "../escape", ""})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "shared" {
		t.Fatalf("bundle link exclusions=%v want only the symlink", got)
	}
}

// TestResolveArchiveLinkExclusionsUseArchiveContents は復元側の除外が archive の持つ path だけで決まることを固定する。
// snapshot 側が symlink として除外した候補だけが archive から欠けるため、両者の判定が一致する。
func TestResolveArchiveLinkExclusionsUseArchiveContents(t *testing.T) {
	t.Parallel()
	file := writeWorkspaceExclusionTestArchive(t, []string{"docs/", "docs/note.md", "AGENTS.md"})

	got, err := resolveArchiveLinkExclusions(file, []string{"shared/inner", "docs", "shared", "docs/note.md", "../escape"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "shared" {
		t.Fatalf("archive link exclusions=%v want only the path missing from the archive", got)
	}
	offset, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatal(err)
	}
	if offset != 0 {
		t.Fatalf("archive offset=%d want the descriptor rewound for the restore pass", offset)
	}
}

// TestResolveArchiveLinkExclusionsTreatDescendantAsPresent は、候補そのものの entry が無くても配下があれば除外しないことを固定する。
// 除外すると prune が残した実体と tar の entry が重なり、復元が overlap エラーになる。
func TestResolveArchiveLinkExclusionsTreatDescendantAsPresent(t *testing.T) {
	t.Parallel()
	file := writeWorkspaceExclusionTestArchive(t, []string{"assets/theme/color.json"})

	got, err := resolveArchiveLinkExclusions(file, []string{"assets"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("archive link exclusions=%v want the ancestor of an archived path kept", got)
	}
}

func writeWorkspaceExclusionTestArchive(t *testing.T, names []string) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "archive.tar")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	writer := tar.NewWriter(file)
	for _, name := range names {
		header := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o600}
		if name[len(name)-1] == '/' {
			header = &tar.Header{Name: name[:len(name)-1], Typeflag: tar.TypeDir, Mode: 0o700}
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	return file
}
