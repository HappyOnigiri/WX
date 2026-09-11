// checkdocsindex はdocs/配下の文書がAGENTS.mdの「作業別ドキュメント」一覧に載っているかを検査する。
// 一覧に無い文書は読む契機を失い、誰も参照しないまま残る。
// 逆向きのリンク切れはmarkdownlintのWX014が見るため、ここでは扱わない。
package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
)

const (
	indexPath     = "AGENTS.md"
	indexHeading  = "## 作業別ドキュメント"
	docsDirectory = "docs"
)

// documentLink は箇条書きからdocs/配下への相対リンクを取り出す。
// アンカー付きの本文中リンクを拾わないよう、対象は箇条書きの行に限る。
var documentLink = regexp.MustCompile(`\]\((docs/[^)#]+\.md)\)`)

type problem struct {
	path    string
	message string
}

func guidance() []string {
	return []string{
		"docs-index guidance:",
		fmt.Sprintf("- Every file under %s/ must appear once in the %q list of %s, so readers can find it from the task at hand.", docsDirectory, indexHeading, indexPath),
		"- Write the entry as `- <観点>を扱うとき: [<表題>](docs/<file>.md)`, matching the surrounding lines.",
		"- A document nobody is pointed at is a document nobody reads; delete it instead of leaving it unlisted.",
	}
}

func main() {
	if err := run(os.DirFS("."), os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(root fs.FS, out io.Writer) error {
	listed, duplicates, err := indexedDocuments(root)
	if err != nil {
		return err
	}
	present, err := presentDocuments(root)
	if err != nil {
		return err
	}
	var problems []problem
	for _, duplicate := range duplicates {
		problems = append(problems, problem{path: indexPath, message: fmt.Sprintf(
			"%s is listed more than once; keep one entry so the reading order stays unambiguous", duplicate)})
	}
	for _, document := range present {
		if listed[document] {
			continue
		}
		problems = append(problems, problem{path: document, message: fmt.Sprintf(
			"this document is missing from the %q list in %s; nobody is told when to read it", indexHeading, indexPath)})
	}
	if len(problems) == 0 {
		_, _ = fmt.Fprintf(out, "checkdocsindex: %d document(s) under %s/ are all listed in %s\n", len(present), docsDirectory, indexPath)
		return nil
	}
	sort.Slice(problems, func(i, j int) bool {
		if problems[i].path != problems[j].path {
			return problems[i].path < problems[j].path
		}
		return problems[i].message < problems[j].message
	})
	for _, item := range problems {
		_, _ = fmt.Fprintf(out, "%s: %s\n", item.path, item.message)
	}
	for _, line := range guidance() {
		_, _ = fmt.Fprintln(out, line)
	}
	return fmt.Errorf("checkdocsindex: %d problem(s); %s and %s/ disagree", len(problems), indexPath, docsDirectory)
}

// indexedDocuments は一覧節の箇条書きが指すdocs/配下のpathと、重複して現れたpathを返す。
// 節が見つからない、または1件も拾えないときは検査が空回りしているので失敗させる。
func indexedDocuments(root fs.FS) (map[string]bool, []string, error) {
	source, err := fs.ReadFile(root, indexPath)
	if err != nil {
		return nil, nil, err
	}
	listed := map[string]bool{}
	var duplicates []string
	inSection := false
	found := false
	for line := range strings.SplitSeq(string(source), "\n") {
		if strings.HasPrefix(line, "#") {
			if inSection {
				break
			}
			inSection = strings.TrimSpace(line) == indexHeading
			found = found || inSection
			continue
		}
		if !inSection || !strings.HasPrefix(strings.TrimSpace(line), "- ") {
			continue
		}
		for _, match := range documentLink.FindAllStringSubmatch(line, -1) {
			if listed[match[1]] {
				duplicates = append(duplicates, match[1])
				continue
			}
			listed[match[1]] = true
		}
	}
	if !found {
		return nil, nil, fmt.Errorf("%s: heading %q is missing; checkdocsindex has nothing to compare against", indexPath, indexHeading)
	}
	if len(listed) == 0 {
		return nil, nil, fmt.Errorf("%s: the %q list links to no document under %s/", indexPath, indexHeading, docsDirectory)
	}
	sort.Strings(duplicates)
	return listed, duplicates, nil
}

func presentDocuments(root fs.FS) ([]string, error) {
	var documents []string
	err := fs.WalkDir(root, docsDirectory, func(target string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(target, ".md") {
			documents = append(documents, target)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(documents)
	return documents, nil
}
