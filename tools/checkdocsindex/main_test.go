package main

import (
	"bytes"
	"strings"
	"testing"
	"testing/fstest"
)

// validIndex は一覧節の前後に別の節と本文中リンクを置き、収集範囲を取り違えないことも確かめる。
const validIndex = `# AGENTS.md

CIのランナーは全てlinuxなので、[部分検証](docs/worktree-copy.md#部分検証)の手順を手元で行う。

## 作業別ドキュメント

変更・調査する観点に対応する文書だけを読む。

- パッケージの責務・依存境界を変えるとき: [責務境界](docs/architecture.md)
- CoW・include・linkの準備処理を扱うとき: [worktreeのコピーとリンク](docs/worktree-copy.md)

## 停止中の自動化

セキュリティ関連は[所有権証明](docs/ownership.md)とは無関係に手動opt-inとする。
`

// repository は一覧とdocs/配下の文書を組み立てる。
func repository(index string, documents ...string) fstest.MapFS {
	files := fstest.MapFS{indexPath: &fstest.MapFile{Data: []byte(index)}}
	for _, document := range documents {
		files[document] = &fstest.MapFile{Data: []byte("# document\n")}
	}
	return files
}

func runCheck(t *testing.T, files fstest.MapFS) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(files, &out)
	return out.String(), err
}

func TestRunAcceptsFullyListedDocuments(t *testing.T) {
	t.Parallel()
	out, err := runCheck(t, repository(validIndex, "docs/architecture.md", "docs/worktree-copy.md"))
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "2 document(s) under docs/ are all listed") {
		t.Fatalf("output=%q", out)
	}
}

func TestRunReportsUnlistedDocument(t *testing.T) {
	t.Parallel()
	out, err := runCheck(t, repository(validIndex, "docs/architecture.md", "docs/worktree-copy.md", "docs/storage-layout.md"))
	if err == nil {
		t.Fatalf("unlisted document accepted (output %q)", out)
	}
	if !strings.Contains(out, "docs/storage-layout.md: this document is missing") {
		t.Fatalf("output=%q", out)
	}
	if strings.Contains(out, "docs/architecture.md:") {
		t.Fatalf("listed document reported: %q", out)
	}
}

// 一覧の外にあるリンクを拾うと、載せ忘れを載っていると誤って判定する。
func TestRunIgnoresLinksOutsideTheSection(t *testing.T) {
	t.Parallel()
	out, err := runCheck(t, repository(validIndex, "docs/architecture.md", "docs/worktree-copy.md", "docs/ownership.md"))
	if err == nil {
		t.Fatalf("document linked only outside the section accepted (output %q)", out)
	}
	if !strings.Contains(out, "docs/ownership.md: this document is missing") {
		t.Fatalf("output=%q", out)
	}
}

func TestRunReportsDuplicateEntry(t *testing.T) {
	t.Parallel()
	index := strings.Replace(validIndex,
		"- CoW・include・linkの準備処理を扱うとき: [worktreeのコピーとリンク](docs/worktree-copy.md)\n",
		"- CoW・include・linkの準備処理を扱うとき: [worktreeのコピーとリンク](docs/worktree-copy.md)\n- 別の観点: [責務境界](docs/architecture.md)\n", 1)
	out, err := runCheck(t, repository(index, "docs/architecture.md", "docs/worktree-copy.md"))
	if err == nil {
		t.Fatalf("duplicate entry accepted (output %q)", out)
	}
	if !strings.Contains(out, "docs/architecture.md is listed more than once") {
		t.Fatalf("output=%q", out)
	}
}

// 見出しや一覧が消えると比較対象が無くなり、検査は何も見ないまま成功してしまう。
func TestRunRefusesAnEmptyIndex(t *testing.T) {
	t.Parallel()
	for name, index := range map[string]string{
		"missing heading": strings.Replace(validIndex, "## 作業別ドキュメント", "## 読む順序", 1),
		"no link":         strings.ReplaceAll(validIndex, "](docs/", "](../"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			files := repository(index, "docs/architecture.md", "docs/worktree-copy.md")
			var out bytes.Buffer
			if err := run(files, &out); err == nil {
				t.Fatalf("%s accepted (output %q)", name, out.String())
			}
		})
	}
}
