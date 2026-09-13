package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/i18n"
)

// probeLocalizationFindings は probe の worktree 検査が作る finding を分岐ごとに 1 件ずつ集める。
func probeLocalizationFindings(t *testing.T) []diag.Finding {
	t.Helper()
	ctx := context.Background()
	git := probeTestGit()
	client := Client{}
	client.Config.Readiness.Timeout.Duration = 30 * time.Second
	notARepository := t.TempDir()
	findings := []diag.Finding{}
	// 読み取れない貸出 path は worktree の列挙そのものを失敗させる。
	findings = append(findings, client.probeWorktreeFindings(ctx, "/root", filepath.Join(notARepository, "missing"))...)
	findings = append(findings, client.probeWorktreeFindings(ctx, "/root", notARepository)...)
	findings = append(findings, probeSubmoduleFindings(ctx, git, "/root", notARepository)...)
	findings = append(findings, probeTrackedFindings(ctx, git, notARepository))

	clean := probeWorktreeFixture(t)
	findings = append(findings, probeSubmoduleFindings(ctx, git, "/root", clean)...)
	findings = append(findings, probeTrackedFindings(ctx, git, clean))
	if err := os.WriteFile(filepath.Join(clean, "tracked"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	findings = append(findings, probeTrackedFindings(ctx, git, clean))

	empty := probeWorktreeFixture(t)
	if err := os.Mkdir(filepath.Join(empty, "vendor"), 0o700); err != nil {
		t.Fatal(err)
	}
	probeGitCommand(t, empty, "update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("a", 40)+",vendor")
	findings = append(findings, probeSubmoduleFindings(ctx, git, "/root", empty)...)

	// index だけが submodule を記録し、directory が無い場合は読み取り自体が失敗する。
	absent := probeWorktreeFixture(t)
	probeGitCommand(t, absent, "update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("b", 40)+",vendor")
	findings = append(findings, probeSubmoduleFindings(ctx, git, "/root", absent)...)
	return findings
}

// 英語で解決した結果は解決前の文字列と一致しなければならない。
// `wx doctor --json` は英語で解決した本文をそのまま載せるため、ここがずれると出力の契約が変わる。
func TestProbeWorktreeFindingsKeepTheirEnglishText(t *testing.T) {
	findings := probeLocalizationFindings(t)
	resolved := diag.Resolve(diag.Reply{Findings: findings}, i18n.English)
	for index, before := range findings {
		after := resolved.Findings[index]
		for _, row := range [][3]string{
			{"summary", before.Summary, after.Summary},
			{"cause", before.Cause, after.Cause},
			{"action", before.Action, after.Action},
		} {
			if row[1] != row[2] {
				t.Fatalf("%s of %q changed when resolved in English:\n before %q\n after  %q", row[0], before.Check, row[1], row[2])
			}
		}
		for detailIndex, detail := range before.Details {
			if detail != after.Details[detailIndex] {
				t.Fatalf("detail %d of %q changed when resolved in English:\n before %q\n after  %q",
					detailIndex, before.Check, detail, after.Details[detailIndex])
			}
		}
	}
}

// 訳されていない finding が残っていないことを、要約と対処が日本語で変わることで確かめる。
func TestProbeWorktreeFindingsHaveNoUntranslatedText(t *testing.T) {
	findings := probeLocalizationFindings(t)
	resolved := diag.Resolve(diag.Reply{Findings: findings}, i18n.Japanese)
	for index, before := range findings {
		after := resolved.Findings[index]
		if before.Summary == after.Summary {
			t.Fatalf("summary of %q was not translated: %q", before.Check, before.Summary)
		}
		if before.Action != "" && before.Action == after.Action {
			t.Fatalf("action of %q was not translated: %q", before.Check, before.Action)
		}
	}
}

// 対象・外部エラーの本文・path は訳さず原文のまま残る。
func TestProbeWorktreeFindingsKeepOpaqueValuesInJapanese(t *testing.T) {
	worktree := probeWorktreeFixture(t)
	probeGitCommand(t, worktree, "update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("c", 40)+",vendor")
	findings := probeSubmoduleFindings(context.Background(), probeTestGit(), "/root", worktree)
	resolved := diag.Resolve(diag.Reply{Findings: findings}, i18n.Japanese).Findings[0]
	if !strings.Contains(resolved.Summary, "submodule") {
		t.Fatalf("summary=%q, want the Japanese text", resolved.Summary)
	}
	for _, fragment := range []string{"/root", "vendor", strings.Repeat("c", 40)} {
		if !strings.Contains(resolved.Cause, fragment) {
			t.Fatalf("cause=%q, want it to keep %q", resolved.Cause, fragment)
		}
	}
}
