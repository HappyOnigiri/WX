package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
	"github.com/HappyOnigiri/WorktreeX/internal/domain"
	"github.com/HappyOnigiri/WorktreeX/internal/gitx"
	"github.com/HappyOnigiri/WorktreeX/internal/state"
)

// recordingMutationHash は fingerprintRootPath の path と内容をそのまま検査するための hash.Hash である。
// hash.Hash の契約だけを実装し、digest の暗号学的な値はこのテストでは使わない。
type recordingMutationHash struct {
	bytes.Buffer
	fail error
}

func (h *recordingMutationHash) Write(value []byte) (int, error) {
	if h.fail != nil {
		return 0, h.fail
	}
	return h.Buffer.Write(value)
}

func (h *recordingMutationHash) ReadFrom(source io.Reader) (int64, error) {
	var total int64
	buffer := make([]byte, 32*1024)
	for {
		count, readErr := source.Read(buffer)
		if count > 0 {
			written, writeErr := h.Write(buffer[:count])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
		}
		if readErr == io.EOF {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

func (h *recordingMutationHash) Sum(value []byte) []byte { return append(value, h.Bytes()...) }
func (h *recordingMutationHash) Reset()                  { h.Buffer.Reset() }
func (h *recordingMutationHash) Size() int               { return sha256.Size }
func (h *recordingMutationHash) BlockSize() int          { return sha256.BlockSize }

var _ hash.Hash = (*recordingMutationHash)(nil)

func TestMutationCopyRootEntryKeepsDirectoryBoundariesAndModes(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	destination := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "settings"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second"} {
		path := filepath.Join(source, "settings", name)
		if err := os.WriteFile(path, []byte(name+"\n"), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(destination, "settings"), 0o700); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(destination, "settings", "first")
	if err := os.WriteFile(existing, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(existing, 0o600); err != nil {
		t.Fatal(err)
	}

	sourceRoot, err := OpenPhysicalRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sourceRoot.Close() }()
	destinationRoot, err := os.OpenRoot(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = destinationRoot.Close() }()
	if err := copyPathFromOwnedRoot(sourceRoot, "settings", destinationRoot, "settings"); err != nil {
		t.Fatalf("copy directory: %v", err)
	}
	for _, name := range []string{"first", "second"} {
		path := filepath.Join(destination, "settings", name)
		content, err := os.ReadFile(path)
		if err != nil || string(content) != name+"\n" {
			t.Fatalf("copied %s content=%q err=%v", name, content, err)
		}
	}
	info, err := os.Stat(existing)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("existing destination mode=%#o want=%#o", got, 0o640)
	}
}

func TestMutationCopyRootEntryPropagatesNestedErrors(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "settings"), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(source, "settings", "socket"))
	if err != nil {
		t.Skipf("unix sockets are unavailable: %v", err)
	}
	defer func() { _ = listener.Close() }()
	destination := t.TempDir()
	sourceRoot, err := OpenPhysicalRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sourceRoot.Close() }()
	destinationRoot, err := os.OpenRoot(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = destinationRoot.Close() }()
	if err := copyPathFromOwnedRoot(sourceRoot, "settings", destinationRoot, "settings"); err == nil {
		t.Fatal("copy succeeded after a nested non-regular source error")
	}
}

// Linux の proc mem は stat では regular file だが read が失敗するため、io.Copy のエラー返却を検査できる。
func TestMutationCopyRootEntryPropagatesCopyErrors(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("/proc/self/mem is Linux-specific")
	}
	sourceRoot, err := OpenPhysicalRoot(fmt.Sprintf("/proc/%d", os.Getpid()))
	if err != nil {
		t.Skipf("proc root unavailable: %v", err)
	}
	defer func() { _ = sourceRoot.Close() }()
	destinationRoot, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = destinationRoot.Close() }()
	if err := copyPathFromOwnedRoot(sourceRoot, "mem", destinationRoot, "mem"); err == nil {
		t.Fatal("copy succeeded despite proc mem read failure")
	}
}

func TestMutationVerifyPinnedRepositoryPathRejectsDifferentDirectory(t *testing.T) {
	t.Parallel()
	pinnedPath := t.TempDir()
	otherPath := t.TempDir()
	root, err := OpenPhysicalRoot(pinnedPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := verifyPinnedRepositoryPath(root, otherPath); err == nil {
		t.Fatal("replaced repository path was accepted")
	}
}

func TestMutationCopyIncludePathKeepsDirectoryBoundaries(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	destination := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "settings"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second"} {
		if err := os.WriteFile(filepath.Join(source, "settings", name), []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sourceRoot, err := OpenPhysicalRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sourceRoot.Close() }()
	destinationRoot, err := os.OpenRoot(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = destinationRoot.Close() }()
	preparer := Preparer{}
	if err := preparer.copyIncludePath(nil, sourceRoot, "settings", destinationRoot, "settings"); err != nil {
		t.Fatalf("copy include directory: %v", err)
	}
	for _, name := range []string{"first", "second"} {
		if content, err := os.ReadFile(filepath.Join(destination, "settings", name)); err != nil || string(content) != name+"\n" {
			t.Fatalf("copied include %s content=%q err=%v", name, content, err)
		}
	}
}

func TestMutationCopyIncludePathPropagatesNestedErrors(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "settings"), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(source, "settings", "socket"))
	if err != nil {
		t.Skipf("unix sockets are unavailable: %v", err)
	}
	defer func() { _ = listener.Close() }()
	destinationRoot, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = destinationRoot.Close() }()
	sourceRoot, err := OpenPhysicalRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sourceRoot.Close() }()
	if err := (&Preparer{}).copyIncludePath(nil, sourceRoot, "settings", destinationRoot, "settings"); err == nil {
		t.Fatal("include succeeded after a nested non-regular source error")
	}
}

func TestMutationRepositoryRootForConfigKeepsValidWorkspaceRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mainPath := filepath.Join(root, "repositories", "app")
	repo := discovery.Repository{MainPath: domain.CanonicalPath(mainPath), RelativePath: filepath.Join("repositories", "app")}
	if got := repositoryRootForConfig(repo); got != root {
		t.Fatalf("repository config root=%q want=%q", got, root)
	}
}

func TestMutationFingerprintOrdersLinkSourcesIndependently(t *testing.T) {
	t.Parallel()
	repository := t.TempDir()
	for _, name := range []string{"a", "z"} {
		if err := os.WriteFile(filepath.Join(repository, name), []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := filepath.Join(repository, ".worktreelink")
	if err := os.WriteFile(manifest, []byte("z\na\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := discovery.Repository{MainPath: domain.CanonicalPath(repository)}
	cfg := config.Defaults()
	first, err := Fingerprint(1, "oid", repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "schema=%d\ngeneration=%d\noid=%s\ncopy_mode=%s\ncow_min_size_kib=%d\n", fingerprintSchemaVersion, 1, "oid", cfg.CopyModeForWorkspaceRepository(string(repo.MainPath), repo.RelativePath, string(repo.MainPath)), cfg.COWMinSizeKiBForWorkspaceRepository(string(repo.MainPath), repo.RelativePath, string(repo.MainPath)))
	for _, name := range []string{".worktreeinclude", ".worktreelink"} {
		var data []byte
		if name == ".worktreelink" {
			data = []byte("z\na\n")
		}
		_, _ = fmt.Fprintf(h, "manifest=%s:%x\n", name, sha256.Sum256(data))
	}
	for _, name := range []string{"a", "z"} {
		_, _ = fmt.Fprintf(h, "worktreelink-source=%s:present=true\n", name)
	}
	_, _ = fmt.Fprintf(h, "workspace-root=%s\ncopy-rules=%q\nlink-rules=%q\nsubmodules=%t\n", string(repo.MainPath), []string(nil), []string(nil), true)
	_, _ = fmt.Fprintln(h, "sparse-checkout-enabled=false")
	expected := fmt.Sprintf("%x", h.Sum(nil))
	if first != expected {
		t.Fatalf("link source order was not normalized: got=%s want=%s", first, expected)
	}
}

func TestMutationSparseCheckoutSettingsInitializesNilRunner(t *testing.T) {
	t.Parallel()
	repository := t.TempDir()
	gitCommand(t, repository, "init", "-b", "main")
	repo := discovery.Repository{MainPath: domain.CanonicalPath(repository)}
	enabled, cone, patterns, err := sparseCheckoutSettings(context.Background(), nil, repo)
	if err != nil || enabled || cone || patterns != nil {
		t.Fatalf("unconfigured sparse checkout enabled=%t cone=%t patterns=%q err=%v", enabled, cone, patterns, err)
	}
}

func TestMutationUpdateCompatibilityFingerprintUsesInheritedSubmodules(t *testing.T) {
	t.Parallel()
	repository := t.TempDir()
	repo := discovery.Repository{MainPath: domain.CanonicalPath(repository)}
	if _, err := UpdateCompatibilityFingerprintWithGit(context.Background(), &gitx.Runner{Timeout: time.Second}, 1, repo, config.Defaults()); err != nil {
		t.Fatalf("inherited submodule setting: %v", err)
	}
}

func TestMutationFingerprintWorkspaceRootRecordsValidLinks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "link"), []byte("link\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := discovery.Repository{MainPath: domain.CanonicalPath(root)}
	cfg := config.Config{Workspaces: map[string]config.Workspace{root: {Link: []string{"link"}}}}
	var h recordingMutationHash
	if err := fingerprintWorkspaceRoot(&h, repo, cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.String(), "workspace-link=link:") {
		t.Fatalf("valid workspace link was not fingerprinted: %q", h.String())
	}
}

func TestMutationFingerprintWorkspaceRootPropagatesNestedErrors(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	listener, err := net.Listen("unix", filepath.Join(root, "socket"))
	if err != nil {
		t.Skipf("unix sockets are unavailable: %v", err)
	}
	defer func() { _ = listener.Close() }()
	repo := discovery.Repository{MainPath: domain.CanonicalPath(root)}
	cfg := config.Config{Workspaces: map[string]config.Workspace{root: {Copy: []string{"socket"}}}}
	var h recordingMutationHash
	if err := fingerprintWorkspaceRoot(&h, repo, cfg); err == nil {
		t.Fatal("workspace fingerprint succeeded after nested source error")
	}
}

func TestMutationFingerprintRootPathKeepsDirectoryBoundaries(t *testing.T) {
	t.Parallel()
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "settings"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second"} {
		if err := os.WriteFile(filepath.Join(rootPath, "settings", name), []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := OpenPhysicalRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	var h recordingMutationHash
	if err := fingerprintRootPath(&h, root, "settings", "settings"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second"} {
		if !strings.Contains(h.String(), filepath.Join("path=settings", name)) {
			t.Fatalf("fingerprint path %s missing from %q", name, h.String())
		}
	}
}

func TestMutationFingerprintRootPathPropagatesHashErrors(t *testing.T) {
	t.Parallel()
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "value"), []byte("value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := OpenPhysicalRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	h := recordingMutationHash{fail: errors.New("hash write failed")}
	if err := fingerprintRootPath(&h, root, "value", "value"); err == nil {
		t.Fatal("fingerprint succeeded after hash copy failure")
	}
}

func TestMutationWritePrepareFingerprintIncludesSinglePrepareFields(t *testing.T) {
	t.Parallel()
	repository := t.TempDir()
	repo := discovery.Repository{MainPath: domain.CanonicalPath(repository)}
	for _, prepare := range []config.Prepare{
		{Version: "v1"},
		{Command: []string{"true"}},
	} {
		cfg := config.Defaults()
		cfg.Repositories[string(repo.MainPath)] = config.Repository{Prepare: prepare}
		var h recordingMutationHash
		if err := writePrepareFingerprint(&h, repo, cfg); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(h.String(), "prepare=") {
			t.Fatalf("prepare=%+v was omitted from fingerprint", prepare)
		}
	}
}

func TestMutationCreatePlannedLinksRevalidatesPinnedMainPath(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	mainPath := filepath.Join(base, "repository")
	replacement := filepath.Join(base, "replacement")
	worktreeRoot := filepath.Join(base, "worktrees")
	target := filepath.Join(worktreeRoot, "target")
	for _, path := range []string{mainPath, replacement, target} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(mainPath, ".gitignore"), []byte("link\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mainPath, "link"), []byte("link\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, mainPath, "init", "-b", "main")
	gitCommand(t, target, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(target, ".gitignore"), []byte("link\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(worktreeRoot, worktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	sourceRoot, err := OpenPhysicalRoot(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sourceRoot.Close() }()
	runner := &gitx.Runner{Timeout: time.Second}
	swapped := false
	runner.SetBeforeRunAtHook(func(args []string) {
		if swapped || len(args) == 0 || args[0] != "check-ignore" {
			return
		}
		if err := os.Rename(mainPath, mainPath+"-old"); err != nil {
			t.Fatalf("rename pinned repository away: %v", err)
		}
		if err := os.Rename(replacement, mainPath); err != nil {
			t.Fatalf("install replacement repository: %v", err)
		}
		swapped = true
	})
	preparer := Preparer{Git: runner, Log: nil}
	repo := discovery.Repository{MainPath: domain.CanonicalPath(mainPath)}
	_, err = preparer.createPlannedLinksAt(context.Background(), repo, sourceRoot, owner, "target", true, []linkSource{{relative: "link", present: true}})
	if !swapped {
		t.Fatal("destination ignore check did not exercise the replacement barrier")
	}
	if err == nil {
		t.Fatal("link creation accepted a replaced main path")
	}
}

func TestMutationNewOwnershipMarkerAcceptsPhysicalDirectory(t *testing.T) {
	t.Parallel()
	target := t.TempDir()
	marker, err := newOwnershipMarker(target, markerFor("slot"), t.TempDir(), false)
	if err != nil {
		t.Fatalf("physical directory marker: %v", err)
	}
	if marker.SlotID != "slot" {
		t.Fatalf("marker=%+v", marker)
	}
}

func TestMutationPlanRootCopiesKeepsAllChildren(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "configs"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second"} {
		if err := os.WriteFile(filepath.Join(source, "configs", name), []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := OpenPhysicalRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	out := map[string]state.Placement{}
	if err := planRootCopies(root, source, "configs", "", out); err != nil {
		t.Fatalf("plan root copies: %v", err)
	}
	for _, name := range []string{"configs/first", "configs/second"} {
		if _, ok := out[name]; !ok {
			t.Fatalf("planned copy %s missing from %#v", name, out)
		}
	}
}
