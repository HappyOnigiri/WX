package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestPrepareCommandSuccessFailureAndTimeout(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	repo := discovery.Repository{MainPath: domain.CanonicalPath(repository)}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = root
	cfg.Repositories = map[string]config.Repository{repository: {Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", "printf ready > marker"}, Timeout: config.Duration{Duration: time.Second}}}}
	successDetails := t.TempDir()
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	preparer := Preparer{Git: &gitx.Runner{Timeout: time.Second}, Config: cfg, DetailDir: successDetails, OwnedRoot: owner, RootPath: root}
	if err := preparer.runPrepareWithIdentity(context.Background(), repo, target, ""); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "marker")); err != nil || string(data) != "ready" {
		t.Fatalf("marker=%q err=%v", data, err)
	}
	if entries, err := os.ReadDir(successDetails); err != nil || len(entries) != 0 {
		t.Fatalf("successful prepare left diagnostics=%v err=%v", entries, err)
	}
	failureDetails := t.TempDir()
	cfg.Repositories[repository] = config.Repository{Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", "exit 7"}, Timeout: config.Duration{Duration: time.Second}}}
	preparer.Config = cfg
	preparer.DetailDir = failureDetails
	err = preparer.runPrepareWithIdentity(context.Background(), repo, target, "")
	var prepareFailure *PrepareCommandError
	if !errors.As(err, &prepareFailure) || prepareFailure.ExitCode != 7 || prepareFailure.TimedOut || prepareFailure.Canceled {
		t.Fatalf("failed prepare error=%v typed=%+v", err, prepareFailure)
	}
	if prepareFailure.FailureID == "" || prepareFailure.DetailPath == "" {
		t.Fatalf("failed prepare did not expose diagnostic identity: %+v", prepareFailure)
	}
	detail, readErr := os.ReadFile(prepareFailure.DetailPath)
	if readErr != nil || !strings.Contains(string(detail), "exit_code: 7") {
		t.Fatalf("failed prepare detail=%q err=%v", detail, readErr)
	}
	if info, statErr := os.Stat(prepareFailure.DetailPath); statErr != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("failed prepare detail mode=%v err=%v", info, statErr)
	}
	cfg.Repositories[repository] = config.Repository{Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", "printf 'unique prepare cause\\n' >&2; exit 17"}, Timeout: config.Duration{Duration: time.Second}}}
	preparer.Config = cfg
	err = preparer.runPrepareWithIdentity(context.Background(), repo, target, "")
	if !errors.As(err, &prepareFailure) || prepareFailure.ExitCode != 17 {
		t.Fatalf("stderr prepare error=%v typed=%+v", err, prepareFailure)
	}
	detail, readErr = os.ReadFile(prepareFailure.DetailPath)
	if readErr != nil || !strings.Contains(string(detail), "unique prepare cause") {
		t.Fatalf("stderr was not retained detail=%q err=%v", detail, readErr)
	}
	cfg.Repositories[repository] = config.Repository{Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", "dd if=/dev/zero bs=1024 count=256 >&2 2>/dev/null; exit 19"}, Timeout: config.Duration{Duration: time.Second}}}
	preparer.Config = cfg
	err = preparer.runPrepareWithIdentity(context.Background(), repo, target, "")
	if !errors.As(err, &prepareFailure) || prepareFailure.ExitCode != 19 {
		t.Fatalf("large output prepare error=%v typed=%+v", err, prepareFailure)
	}
	if info, statErr := os.Stat(prepareFailure.DetailPath); statErr != nil || info.Size() > maxPrepareDiagnosticOutput+4096 {
		t.Fatalf("large output detail size=%v err=%v", info, statErr)
	}
	cfg.Repositories[repository] = config.Repository{Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", "sleep 2"}, Timeout: config.Duration{Duration: 20 * time.Millisecond}}}
	preparer.Config = cfg
	err = preparer.runPrepareWithIdentity(context.Background(), repo, target, "")
	if !errors.As(err, &prepareFailure) || !prepareFailure.TimedOut || prepareFailure.Canceled {
		t.Fatalf("timed out prepare error=%v typed=%+v", err, prepareFailure)
	}
	cfg.Repositories[repository] = config.Repository{Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", "sleep 2"}, Timeout: config.Duration{Duration: time.Second}}}
	preparer.Config = cfg
	cancelCtx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	err = preparer.runPrepareWithIdentity(cancelCtx, repo, target, "")
	if !errors.As(err, &prepareFailure) || prepareFailure.TimedOut || !prepareFailure.Canceled {
		t.Fatalf("canceled prepare error=%v typed=%+v", err, prepareFailure)
	}
	cancel()
	cfg.Repositories[repository] = config.Repository{Prepare: config.Prepare{Command: []string{"/path/to/missing-wx-prepare-command"}, Timeout: config.Duration{Duration: time.Second}}}
	preparer.Config = cfg
	err = preparer.runPrepareWithIdentity(context.Background(), repo, target, "")
	if !errors.As(err, &prepareFailure) || prepareFailure.ExitCode != -1 || prepareFailure.DetailPath == "" {
		t.Fatalf("missing executable prepare error=%v typed=%+v", err, prepareFailure)
	}
	if detail, readErr := os.ReadFile(prepareFailure.DetailPath); readErr != nil || !strings.Contains(string(detail), "start_error:") {
		t.Fatalf("missing executable detail=%q err=%v", detail, readErr)
	}
	cfg.Repositories[repository] = config.Repository{Prepare: config.Prepare{Command: []string{"/bin/true"}, Timeout: config.Duration{Duration: -time.Second}}}
	preparer.Config = cfg
	err = preparer.runPrepareWithIdentity(context.Background(), repo, target, "")
	if !errors.As(err, &prepareFailure) || !strings.Contains(err.Error(), "timeout must not be negative") {
		t.Fatalf("invalid timeout prepare error=%v typed=%+v", err, prepareFailure)
	}
}

func TestPrepareCommandErrorUnwrapAndNilReceiver(t *testing.T) {
	cause := errors.New("prepare cause")
	failure := &PrepareCommandError{Err: fmt.Errorf("wrapped: %w", cause)}
	if !errors.Is(failure, cause) {
		t.Fatalf("prepare command error did not unwrap cause: %v", failure)
	}
	var nilFailure *PrepareCommandError
	if nilFailure.Unwrap() != nil || nilFailure.Error() != "prepare command failed" || errors.Is(nilFailure, cause) {
		t.Fatalf("nil prepare command error is not safe: unwrap=%v error=%q is=%v", nilFailure.Unwrap(), nilFailure.Error(), errors.Is(nilFailure, cause))
	}
}

// TestRunPrepareWithIdentityForcesDescriptorPathWhenIdentityExpectedは、非空identityがdescriptor-bound command経路を強制する分岐を確認する。
// preparerがunpinnedでも設定rootが未作成ならopenに失敗し、identityを無視する通常のexec.Commandへ黙ってfallbackしてはならない。
func TestRunPrepareWithIdentityForcesDescriptorPathWhenIdentityExpected(t *testing.T) {
	_, repo, preparer, _, target := prepareEdgesFixture(t)
	cfg := preparer.Config
	cfg.Repositories = map[string]config.Repository{string(repo.MainPath): {Prepare: config.Prepare{Command: []string{"/bin/true"}, Timeout: config.Duration{Duration: time.Second}}}}
	preparer.Config = cfg
	if err := preparer.runPrepareWithIdentity(context.Background(), repo, target, "some-identity"); err == nil {
		t.Fatal("descriptor-bound prepare command with a missing configured root succeeded")
	}
}

// TestRunPrepareWithIdentityPropagatesTargetOpenFailureは、descriptor-bound経路でtarget openが失敗する分岐を確認する。
// 設定rootは存在してopenOwnedRootが成功するが、targetは存在しないためディレクトリopenに失敗する。
func TestRunPrepareWithIdentityPropagatesTargetOpenFailure(t *testing.T) {
	_, repo, preparer, _, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := preparer.Config
	cfg.Repositories = map[string]config.Repository{string(repo.MainPath): {Prepare: config.Prepare{Command: []string{"/bin/true"}, Timeout: config.Duration{Duration: time.Second}}}}
	preparer.Config = cfg
	if err := preparer.runPrepareWithIdentity(context.Background(), repo, target, "some-identity"); err == nil {
		t.Fatal("descriptor-bound prepare command opened a missing target")
	}
}

// TestRunPrepareWithIdentityDetectsTargetReplacementDuringCommandは、command後のidentity検査を確認する。
// prepare commandが終了前に同じpathのtargetを別directoryへ置換した場合、元のままではなく所有権不確かなidentity変更として検出する。
func TestRunPrepareWithIdentityDetectsTargetReplacementDuringCommand(t *testing.T) {
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := preparer.Prepare(context.Background(), repo, target, head, "slot"); err != nil {
		t.Fatal(err)
	}
	identity, err := preparer.WorktreeIdentity(target)
	if err != nil {
		t.Fatal(err)
	}
	// commandは終了前に自身のworking directoryを同名の新しい空directoryへ置換する。
	// そのためcmd.Run()の返却時にはtargetが別の物理inodeを指す。
	script := "parent=$(dirname \"$PWD\"); name=$(basename \"$PWD\"); cd \"$parent\" && rm -rf \"$name\" && mkdir \"$name\""
	cfg := preparer.Config
	cfg.Repositories = map[string]config.Repository{string(repo.MainPath): {Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", script}, Timeout: config.Duration{Duration: 5 * time.Second}}}}
	preparer.Config = cfg
	if err := preparer.runPrepareWithIdentity(context.Background(), repo, target, identity); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("target replacement during the prepare command was not detected: %v", err)
	}
}

func TestPinnedPrepareCommandRunsInsideValidatedWorktree(t *testing.T) {
	ctx := context.Background()
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	preparer.OwnedRoot = owner
	preparer.RootPath = root
	if err := preparer.Prepare(ctx, repo, target, head, "prepare-command"); err != nil {
		t.Fatal(err)
	}
	identity, err := preparer.WorktreeIdentity(target)
	if err != nil {
		t.Fatal(err)
	}
	cfg := preparer.Config
	cfg.Repositories = map[string]config.Repository{string(repo.MainPath): {Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", "printf pinned > prepare-marker"}}}}
	preparer.Config = cfg
	if err := preparer.runPrepareWithIdentity(ctx, repo, target, identity); err != nil {
		t.Fatalf("descriptor-bound prepare command: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "prepare-marker")); err != nil || string(data) != "pinned" {
		t.Fatalf("prepare marker=%q err=%v", data, err)
	}
	// 0以下のコマンドタイムアウトは設定済みの準備待ち予算へフォールバックし、
	// 通常経路も検証対象に含める。
	cfg.Repositories[string(repo.MainPath)] = config.Repository{Prepare: config.Prepare{Command: []string{"/usr/bin/true"}}}
	preparer.Config = cfg
	plain := &Preparer{Git: preparer.Git, Config: cfg, OwnedRoot: owner, RootPath: root}
	if err := plain.runPrepareWithIdentity(ctx, repo, target, ""); err != nil {
		t.Fatalf("default prepare timeout: %v", err)
	}
	// フォールバック先は repository 個別の readiness.timeout である。
	// global だけを見ていると、遅い repository のために伸ばした待機予算が prepare command へ届かない。
	cfg.Readiness.Timeout = config.Duration{}
	cfg.Repositories[string(repo.MainPath)] = config.Repository{
		Prepare:   config.Prepare{Command: []string{"/usr/bin/true"}},
		Readiness: config.RepositoryReadiness{Timeout: &config.Duration{Duration: time.Minute}},
	}
	perRepository := &Preparer{Git: preparer.Git, Config: cfg, OwnedRoot: owner, RootPath: root}
	if err := perRepository.runPrepareWithIdentity(ctx, repo, target, ""); err != nil {
		t.Fatalf("repository readiness timeout fallback: %v", err)
	}
	// 個別指定の無い repository は global の 0 のまま失敗し、フォールバック元が変わっていないことを示す。
	other := discovery.Repository{MainPath: repo.MainPath + "-other"}
	cfg.Repositories[string(other.MainPath)] = config.Repository{Prepare: config.Prepare{Command: []string{"/usr/bin/true"}}}
	if err := perRepository.runPrepareWithIdentity(ctx, other, target, ""); err == nil {
		t.Fatal("a repository without an override accepted a non-positive timeout")
	}
}
