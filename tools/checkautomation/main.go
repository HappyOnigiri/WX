// checkautomation は停止中のセキュリティ・SBOM関連targetが自動実行経路へ戻っていないかを検査する。
// 名前の出現ではなく接続を見るため、Makefileの依存閉包・security.ymlのトリガー・
// 他のworkflowとscripts・hookcheckからの参照を1本の検査にまとめる。
package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	makefilePath         = "Makefile"
	workflowDirectory    = ".github/workflows"
	securityWorkflowPath = ".github/workflows/security.yml"
)

// referenceRoots は対象targetの参照を探す範囲。security.ymlは手動実行の入口なので別に扱う。
var referenceRoots = []string{workflowDirectory, "scripts", "tools/hookcheck"}

// pausedTargets は手動opt-inに留めるtarget。
// 自動実行経路へ繋がった瞬間に落とすのが目的で、target自体の実行は禁じない。
var pausedTargets = []string{
	"setup-security-tools",
	"setup-sbom-tools",
	"govulncheck",
	"dependency-check",
	"gosec",
	"license-check",
	"secret-check",
	"sbom",
	"security-local",
}

// automationEntries は自動実行の入口。ciはrecipeの$(MAKE)でci-checksを呼ぶため、両方を起点に置く。
var automationEntries = []string{
	"ci",
	"ci-checks",
	"static-check",
	"check-fast",
	"setup",
	"hook-pre-commit",
}

// allowedSecurityTriggers はsecurity.ymlに許すトリガー。手動実行だけを残す。
var allowedSecurityTriggers = map[string]bool{"workflow_dispatch": true}

type problem struct {
	path    string
	line    int
	message string
}

func guidance() []string {
	return []string{
		"paused-automation guidance:",
		"- Security and SBOM targets stay manual opt-in; re-enabling any automatic trigger needs the user's explicit permission.",
		fmt.Sprintf("- Keep them out of the dependency closure of %s, and out of %s triggers other than workflow_dispatch.", strings.Join(automationEntries, ", "), path.Base(securityWorkflowPath)),
		fmt.Sprintf("- %s may name these targets because it only runs on demand; no other workflow, script, or hook selection may.", securityWorkflowPath),
	}
}

func main() {
	if err := run(os.DirFS("."), os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(root fs.FS, out io.Writer) error {
	source, err := fs.ReadFile(root, makefilePath)
	if err != nil {
		return err
	}
	parsed := parseMakefile(source)
	problems := append(checkNames(parsed), checkClosure(parsed)...)
	triggerProblems, err := checkSecurityTriggers(root)
	if err != nil {
		return err
	}
	problems = append(problems, triggerProblems...)
	referenceProblems, err := checkReferences(root)
	if err != nil {
		return err
	}
	problems = append(problems, referenceProblems...)
	sort.Slice(problems, func(i, j int) bool {
		if problems[i].path != problems[j].path {
			return problems[i].path < problems[j].path
		}
		if problems[i].line != problems[j].line {
			return problems[i].line < problems[j].line
		}
		return problems[i].message < problems[j].message
	})
	if len(problems) == 0 {
		_, _ = fmt.Fprintf(out, "checkautomation: %d paused target(s) stay outside the %d automation entry point(s) and trigger only on demand\n",
			len(pausedTargets), len(automationEntries))
		return nil
	}
	for _, item := range problems {
		if item.line > 0 {
			_, _ = fmt.Fprintf(out, "%s:%d: %s\n", item.path, item.line, item.message)
			continue
		}
		_, _ = fmt.Fprintf(out, "%s: %s\n", item.path, item.message)
	}
	for _, line := range guidance() {
		_, _ = fmt.Fprintln(out, line)
	}
	return fmt.Errorf("checkautomation: %d problem(s); paused security/SBOM automation is wired back in", len(problems))
}

// makefile はtargetごとの依存先を持つ。prerequisiteとrecipe中の$(MAKE)呼び出しを同じ辺として扱う。
type makefile struct {
	dependencies map[string][]string
	defined      map[string]bool
}

// checkNames は検査対象のtarget名がMakefileに実在するかを確かめる。
// 改名や削除で名前が合わなくなると、以降の検査が何も見ないまま成功してしまう。
func checkNames(parsed makefile) []problem {
	var problems []problem
	for _, group := range []struct {
		names []string
		role  string
	}{
		{pausedTargets, "paused security/SBOM target"},
		{automationEntries, "automation entry point"},
	} {
		for _, name := range group.names {
			if parsed.defined[name] {
				continue
			}
			problems = append(problems, problem{path: makefilePath, message: fmt.Sprintf(
				"%s %q is listed by checkautomation but no longer defined here; update the list or this check silently passes", group.role, name)})
		}
	}
	return problems
}

// checkClosure は自動実行の入口から辿れる依存閉包に対象targetが含まれないかを確かめる。
func checkClosure(parsed makefile) []problem {
	chains := reachable(parsed, automationEntries)
	var problems []problem
	for _, name := range pausedTargets {
		chain, found := chains[name]
		if !found {
			continue
		}
		problems = append(problems, problem{path: makefilePath, message: fmt.Sprintf(
			"paused target %q is reachable from the automation entry points via %s; keep it manual opt-in", name, strings.Join(chain, " -> "))})
	}
	return problems
}

// reachable は起点から辿れるtargetと、最初に見つけた経路を返す。
func reachable(parsed makefile, entries []string) map[string][]string {
	chains := map[string][]string{}
	var queue [][]string
	for _, entry := range entries {
		if _, seen := chains[entry]; seen {
			continue
		}
		chains[entry] = []string{entry}
		queue = append(queue, []string{entry})
	}
	for len(queue) > 0 {
		chain := queue[0]
		queue = queue[1:]
		for _, next := range parsed.dependencies[chain[len(chain)-1]] {
			if _, seen := chains[next]; seen {
				continue
			}
			extended := append(append([]string{}, chain...), next)
			chains[next] = extended
			queue = append(queue, extended)
		}
	}
	return chains
}

// subMake はrecipeからの再帰呼び出しの目印。
const subMake = "$(MAKE)"

// targetToken はtarget名として扱えるトークン。変数参照・オプション・変数代入を弾く。
var targetToken = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// parseMakefile はtarget行のprerequisiteとrecipe中の$(MAKE)だけを辿る簡易な解析を行う。
// 条件分岐や変数展開は解釈せず、静的に読める辺だけを集める。
func parseMakefile(source []byte) makefile {
	parsed := makefile{dependencies: map[string][]string{}, defined: map[string]bool{}}
	var current []string
	for raw := range strings.SplitSeq(string(source), "\n") {
		if strings.HasPrefix(raw, "\t") {
			for _, name := range subMakeTargets(raw) {
				for _, target := range current {
					parsed.dependencies[target] = append(parsed.dependencies[target], name)
				}
			}
			continue
		}
		names, prerequisites, ok := targetLine(raw)
		if !ok {
			current = nil
			continue
		}
		current = names
		for _, name := range names {
			parsed.defined[name] = true
			parsed.dependencies[name] = append(parsed.dependencies[name], prerequisites...)
		}
	}
	return parsed
}

// targetLine はtarget定義行を分解する。変数代入と、全targetを列挙する.PHONYなどの特別targetは対象外にする。
func targetLine(raw string) (names, prerequisites []string, ok bool) {
	line := strings.TrimRight(raw, " \t\r")
	if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, " ") {
		return nil, nil, false
	}
	colon := strings.Index(line, ":")
	if colon < 0 || strings.Contains(line[:colon], "=") {
		return nil, nil, false
	}
	rest := strings.TrimPrefix(line[colon+1:], ":")
	if strings.HasPrefix(rest, "=") {
		return nil, nil, false
	}
	names = strings.Fields(line[:colon])
	if len(names) == 0 || strings.HasPrefix(names[0], ".") {
		return nil, nil, false
	}
	if comment := strings.Index(rest, "#"); comment >= 0 {
		rest = rest[:comment]
	}
	for token := range strings.FieldsSeq(rest) {
		if targetToken.MatchString(token) {
			prerequisites = append(prerequisites, token)
		}
	}
	return names, prerequisites, true
}

// subMakeTargets はrecipe行の$(MAKE)が呼ぶtarget名を返す。区切り文字までを1回の呼び出しとみなす。
func subMakeTargets(raw string) []string {
	var result []string
	for _, rest := range strings.Split(raw, subMake)[1:] {
		for token := range strings.FieldsSeq(rest) {
			stop := false
			if cut := strings.IndexAny(token, ";&|"); cut >= 0 {
				token, stop = token[:cut], true
			}
			if targetToken.MatchString(token) {
				result = append(result, token)
			}
			if stop {
				break
			}
		}
	}
	return result
}

// checkSecurityTriggers はsecurity.ymlのon:が手動実行だけかを確かめる。
// on はYAMLのboolへ解決され得るため、キーは生の文字列としてNodeから読む。
func checkSecurityTriggers(root fs.FS) ([]problem, error) {
	source, err := fs.ReadFile(root, securityWorkflowPath)
	if err != nil {
		return nil, err
	}
	var document yaml.Node
	if err := yaml.Unmarshal(source, &document); err != nil {
		return nil, fmt.Errorf("parse %s: %w", securityWorkflowPath, err)
	}
	node := mappingValue(documentRoot(&document), "on")
	if node == nil {
		return []problem{{path: securityWorkflowPath, message: "the on: key is missing; this check cannot confirm that the workflow only runs on demand"}}, nil
	}
	triggers := triggerNames(node)
	if len(triggers) == 0 {
		return []problem{{path: securityWorkflowPath, line: node.Line, message: "the on: key lists no trigger this check can read; keep it a plain workflow_dispatch entry"}}, nil
	}
	var problems []problem
	for _, trigger := range triggers {
		if allowedSecurityTriggers[trigger.name] {
			continue
		}
		problems = append(problems, problem{path: securityWorkflowPath, line: trigger.line, message: fmt.Sprintf(
			"trigger %q restarts the paused security workflow automatically; only workflow_dispatch is allowed", trigger.name)})
	}
	return problems, nil
}

type trigger struct {
	name string
	line int
}

func documentRoot(document *yaml.Node) *yaml.Node {
	if document.Kind == yaml.DocumentNode && len(document.Content) == 1 {
		return document.Content[0]
	}
	return document
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			return node.Content[index+1]
		}
	}
	return nil
}

// triggerNames はon:の3つの記法（scalar・sequence・mapping）からトリガー名を集める。
func triggerNames(node *yaml.Node) []trigger {
	if node.Kind == yaml.ScalarNode {
		if node.Value == "" {
			return nil
		}
		return []trigger{{name: node.Value, line: node.Line}}
	}
	// sequenceは要素ごと、mappingはキーだけがトリガー名になるため、進める幅を分ける。
	step := 1
	if node.Kind == yaml.MappingNode {
		step = 2
	} else if node.Kind != yaml.SequenceNode {
		return nil
	}
	var result []trigger
	for index := 0; index < len(node.Content); index += step {
		result = append(result, trigger{name: node.Content[index].Value, line: node.Content[index].Line})
	}
	return result
}

// checkReferences は対象target名が自動実行側の入力に現れないかを確かめる。
// security.ymlは手動実行専用なのでファイル名で除外し、他のworkflowへは漏らさない。
func checkReferences(root fs.FS) ([]problem, error) {
	patterns := make(map[string]*regexp.Regexp, len(pausedTargets))
	for _, name := range pausedTargets {
		patterns[name] = regexp.MustCompile(`(^|[^A-Za-z0-9_.-])` + regexp.QuoteMeta(name) + `($|[^A-Za-z0-9_.-])`)
	}
	var problems []problem
	for _, directory := range referenceRoots {
		err := fs.WalkDir(root, directory, func(target string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || target == securityWorkflowPath {
				return nil
			}
			source, err := fs.ReadFile(root, target)
			if err != nil {
				return err
			}
			problems = append(problems, referenceProblems(target, string(source), patterns)...)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return problems, nil
}

func referenceProblems(target, source string, patterns map[string]*regexp.Regexp) []problem {
	var problems []problem
	for index, line := range strings.Split(source, "\n") {
		for _, name := range pausedTargets {
			if !patterns[name].MatchString(line) {
				continue
			}
			problems = append(problems, problem{path: target, line: index + 1, message: fmt.Sprintf(
				"paused target %q is named here; only %s may run it, and only on demand", name, securityWorkflowPath)})
		}
	}
	return problems
}
