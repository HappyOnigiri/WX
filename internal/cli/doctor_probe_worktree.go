package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/i18n"
)

// gitlinkIndexMode は index が submodule の commit を指す entry の mode である。
const gitlinkIndexMode = "160000"

// probeWorktreeFindings は貸し出した worktree が実際に使える状態かを読み取りだけで確かめる。
// エージェントは wx が作った worktree で作業するため、ここで見るのはその前提が成り立っているかである。
func (c Client) probeWorktreeFindings(ctx context.Context, root, leasePath string) []diag.Finding {
	return c.probeWorktreeFindingsWithExclusions(ctx, root, leasePath, nil)
}

func (c Client) probeWorktreeFindingsWithExclusions(ctx context.Context, root, leasePath string, excluded map[string]bool) []diag.Finding {
	return c.probeWorktreeFindingsWithOptions(ctx, root, leasePath, excluded, true)
}

func (c Client) probeWorktreeFindingsWithOptions(ctx context.Context, root, leasePath string, excluded map[string]bool, checkSubmodules bool) []diag.Finding {
	git := c.probeGit()
	worktrees, err := probeWorktrees(ctx, git, leasePath)
	if err != nil {
		return []diag.Finding{{
			Check: diag.CheckProbe, Severity: diag.SeverityUnchecked, Summary: "the prepared worktree could not be inspected",
			Target: leasePath, Cause: err.Error(),
			Action:    fmt.Sprintf("check that %s is readable by you and that its volume is mounted, then run wx doctor --probe again", leasePath),
			DependsOn: diag.CheckProbe,
			Messages: diag.FindingMessages{
				Summary: message("diag.probe.worktree_uninspectable"),
				Action:  message("diag.action.probe_check_lease_path", "Path", leasePath),
			},
		}}
	}
	if len(worktrees) == 0 {
		return []diag.Finding{{
			Check: diag.CheckProbe, Severity: diag.SeverityProblem, Summary: "the prepared workspace holds no Git worktree",
			Target: leasePath,
			Cause:  fmt.Sprintf("wx reported the slot leased for %s as ready, but neither the leased path nor any directory in it is the top level of a Git worktree", root),
			Action: "run wx doctor --probe -v to see the preparation phases, then check the source repositories of this workspace",
			Messages: diag.FindingMessages{
				Summary: message("diag.probe.no_worktree"),
				Cause:   message("diag.probe.no_worktree_cause", "Root", root),
				Action:  message("diag.action.probe_check_sources"),
			},
		}}
	}
	findings := []diag.Finding{}
	for _, worktree := range worktrees {
		if checkSubmodules {
			findings = append(findings, probeSubmoduleFindings(ctx, git, root, worktree, excluded)...)
		}
		findings = append(findings, probeTrackedFindings(ctx, git, worktree))
	}
	return findings
}

// probeWorktrees は貸出単位の中にある repository worktree の path を返す。
// 単一リポジトリ workspace では貸出 path そのもの、multi_repository ではその直下の各 repository が対象になる。
func probeWorktrees(ctx context.Context, git *gitx.Runner, leasePath string) ([]string, error) {
	if worktreeTopLevel(ctx, git, leasePath) {
		return []string{leasePath}, nil
	}
	entries, err := os.ReadDir(leasePath)
	if err != nil {
		return nil, err
	}
	worktrees := []string{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		child := filepath.Join(leasePath, entry.Name())
		if worktreeTopLevel(ctx, git, child) {
			worktrees = append(worktrees, child)
		}
	}
	sort.Strings(worktrees)
	return worktrees, nil
}

// worktreeTopLevel は path が Git worktree の最上位かを返す。
// 最上位に限るのは、repository の内側の任意の directory も Git から見れば repository の中になるためである。
func worktreeTopLevel(ctx context.Context, git *gitx.Runner, path string) bool {
	result, err := git.Run(ctx, path, "rev-parse", "--show-toplevel")
	if err != nil {
		return false
	}
	toplevel, err := filepath.EvalSymlinks(strings.TrimSpace(result.Stdout))
	if err != nil {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	return toplevel == resolved
}

// probeSubmoduleFindings は index が commit を指しているのに実体が空の submodule を報告する。
// 準備完了時の tracked-status 検査は未初期化 submodule を変更と見なさないため、この状態は準備を通り抜けて READY になる。
func probeSubmoduleFindings(ctx context.Context, git *gitx.Runner, root, worktree string, excluded ...map[string]bool) []diag.Finding {
	result, err := git.Run(ctx, worktree, "ls-files", "--stage", "-z")
	if err != nil {
		return []diag.Finding{{
			Check: diag.CheckProbeSubmodule, Severity: diag.SeverityUnchecked, Summary: "the submodules of a prepared worktree could not be checked",
			Target: worktree, Cause: err.Error(),
			Action:    fmt.Sprintf("run git -C %s ls-files --stage yourself to see why Git fails there, then run wx doctor --probe again", worktree),
			DependsOn: diag.CheckProbe,
			Messages: diag.FindingMessages{
				Summary: message("diag.probe.submodules_unchecked"),
				Action:  message("diag.action.probe_run_git_ls_files", "Path", worktree),
			},
		}}
	}
	findings := []diag.Finding{}
	gitlinks := parseGitlinks(result.Stdout)
	excludedCount := 0
	excludedPaths := map[string]bool{}
	if len(excluded) > 0 && excluded[0] != nil {
		excludedPaths = excluded[0]
	}
	for _, gitlink := range gitlinks {
		if excludedPaths[gitlink.path] {
			excludedCount++
			continue
		}
		populated, err := directoryPopulated(filepath.Join(worktree, gitlink.path))
		if err != nil {
			findings = append(findings, diag.Finding{
				Check: diag.CheckProbeSubmodule, Severity: diag.SeverityProblem, Summary: "a submodule of the prepared worktree is missing",
				Target: filepath.Join(worktree, gitlink.path),
				Cause: fmt.Sprintf("the index of the worktree prepared for %s records the submodule at %s as commit %s, but its directory could not be read: %v",
					root, gitlink.path, gitlink.oid, err),
				Action: submoduleAction(root, gitlink.path),
				Messages: diag.FindingMessages{
					Summary: message("diag.probe.submodule_missing"),
					Cause: message("diag.probe.submodule_missing_cause",
						"Root", root, "Path", gitlink.path, "Commit", gitlink.oid, "Error", err.Error()),
					Action: submoduleActionMessage(root, gitlink.path),
				},
			})
			continue
		}
		if populated {
			continue
		}
		findings = append(findings, diag.Finding{
			Check: diag.CheckProbeSubmodule, Severity: diag.SeverityProblem, Summary: "a submodule of the prepared worktree is empty",
			Target: filepath.Join(worktree, gitlink.path),
			Cause: fmt.Sprintf("the index of the worktree prepared for %s records the submodule at %s as commit %s, but the directory wx prepared for it is empty, so an agent working there sees no submodule content",
				root, gitlink.path, gitlink.oid),
			Action: submoduleAction(root, gitlink.path),
			Messages: diag.FindingMessages{
				Summary: message("diag.probe.submodule_empty"),
				Cause: message("diag.probe.submodule_empty_cause",
					"Root", root, "Path", gitlink.path, "Commit", gitlink.oid),
				Action: submoduleActionMessage(root, gitlink.path),
			},
		})
	}
	if len(findings) > 0 {
		return findings
	}
	details := []string{fmt.Sprintf("%d submodule(s) checked", len(gitlinks)-excludedCount)}
	detailMessages := []i18n.Message{message("diag.detail.submodules_checked", "Count", len(gitlinks)-excludedCount)}
	if excludedCount > 0 {
		details = append(details, fmt.Sprintf("%d submodule(s) were outside the preparation range", excludedCount))
		detailMessages = append(detailMessages, message("diag.detail.submodules_out_of_scope", "Count", excludedCount))
	}
	return []diag.Finding{{
		Check: diag.CheckProbeSubmodule, Severity: diag.SeverityOK,
		Summary: "every submodule recorded in the index has content", Target: worktree,
		Details: details,
		Messages: diag.FindingMessages{
			Summary: message("diag.probe.submodules_ok"),
			Details: detailMessages,
		},
	}}
}

// submoduleAction は未初期化 submodule への対処を、wx が肩代わりしないことを含めて示す。
func submoduleAction(root, submodule string) string {
	return fmt.Sprintf("initialize %s in the source repository of %s so the preparation can place it, or stop the post-checkout hook from expecting it in wx worktrees; wx does not populate submodules itself",
		submodule, root)
}

// submoduleActionMessage は submoduleAction と同じ対処を表示言語で解決するための message である。
func submoduleActionMessage(root, submodule string) i18n.Message {
	return message("diag.action.probe_init_submodule", "Path", submodule, "Root", root)
}

// gitlinkEntry は index の gitlink 1 件の path と commit である。
type gitlinkEntry struct {
	path string
	oid  string
}

// parseGitlinks は `git ls-files --stage -z` の出力から gitlink entry を取り出す。
func parseGitlinks(output string) []gitlinkEntry {
	entries := []gitlinkEntry{}
	for entry := range strings.SplitSeq(output, "\x00") {
		if entry == "" {
			continue
		}
		metadata, path, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(metadata)
		if !ok || len(fields) != 3 || fields[0] != gitlinkIndexMode {
			continue
		}
		entries = append(entries, gitlinkEntry{path: path, oid: fields[1]})
	}
	return entries
}

// directoryPopulated は directory が 1 件でも entry を持つかを返す。
func directoryPopulated(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}
	return len(entries) > 0, nil
}

// probeTrackedFindings は準備し終えた worktree に追跡ファイルの変更が残っていないかを見る。
// 準備は完了時に同じ検査を通しているため、ここで差分が出るのは準備後に worktree が書き換わったことを意味する。
func probeTrackedFindings(ctx context.Context, git *gitx.Runner, worktree string) diag.Finding {
	result, err := git.Run(ctx, worktree, "status", "--porcelain=v1", "--untracked-files=no")
	if err != nil {
		return diag.Finding{
			Check: diag.CheckProbeTracked, Severity: diag.SeverityUnchecked, Summary: "the tracked files of a prepared worktree could not be checked",
			Target: worktree, Cause: err.Error(),
			Action:    fmt.Sprintf("run git -C %s status yourself to see why Git fails there, then run wx doctor --probe again", worktree),
			DependsOn: diag.CheckProbe,
			Messages: diag.FindingMessages{
				Summary: message("diag.probe.tracked_unchecked"),
				Action:  message("diag.action.probe_run_git_status", "Path", worktree),
			},
		}
	}
	changes := strings.TrimSpace(result.Stdout)
	if changes == "" {
		return diag.Finding{
			Check: diag.CheckProbeTracked, Severity: diag.SeverityOK,
			Summary: "the prepared worktree has no tracked change", Target: worktree,
			Messages: diag.FindingMessages{Summary: message("diag.probe.tracked_clean")},
		}
	}
	return diag.Finding{
		Check: diag.CheckProbeTracked, Severity: diag.SeverityProblem, Summary: "the prepared worktree already has tracked changes",
		Target: worktree,
		Cause:  "git status reports modified tracked files in a worktree wx just prepared, so an agent would start on a base that is not the requested commit: " + changes,
		Action: "check the post-checkout hook and the prepare command of this workspace for writes to tracked files, then run wx doctor --probe again",
		Messages: diag.FindingMessages{
			Summary: message("diag.probe.tracked_changes"),
			Cause:   message("diag.probe.tracked_changes_cause", "Changes", changes),
			Action:  message("diag.action.probe_check_prepare_writes"),
		},
	}
}
