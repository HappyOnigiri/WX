package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/HappyOnigiri/WX/internal/gitx"
)

const (
	branchAttachSystemMessage = "wx blocked a branch attach in a linked worktree."
	branchAttachReason        = "The command attaches a branch in a linked worktree. " +
		"Linked worktrees stay detached while you edit and commit, and branches are published as refs instead of being attached to the working tree. " +
		"Keep working detached. To publish, run `git branch <branch> HEAD` and then `git push -u origin <branch>`. " +
		"To update an existing branch, name the destination explicitly: `git push origin HEAD:refs/heads/<branch>`. " +
		"If HEAD is already attached, run `git switch --detach` first."
	branchPolicyUnresolvedSystemMessage = "wx blocked a git command whose effect on a linked worktree it cannot determine."
	branchPolicyUnresolvedReason        = "wx cannot statically tell where cd, git -C or a nested shell runs this git command, " +
		"or cannot confirm that it does not attach a branch, so it fails closed rather than miss a branch attach in a linked worktree. " +
		"Write a literal cd or git -C followed by the git command, without sh -c, pushd or variables. " +
		"If an earlier command in the same call clones or adds the directory the git command uses, " +
		"split the creation and the following git commands into two Bash calls."
)

// branchPolicyGitTimeout は判定用の git 1 回あたりの上限である。固まった 1 回で hook の持ち時間を使い切らないため。
const branchPolicyGitTimeout = 10 * time.Second

// branchPolicySubcommands はブランチを作業ツリーへ attach し得る subcommand である。
// git worktree add は worktreeAddHookOutput が扱うので含めない。
var branchPolicySubcommands = map[string]bool{"checkout": true, "switch": true, "symbolic-ref": true}

// branchPolicyGitCommand は command 文字列に対象の git 呼び出しらしい形があるかを広めに拾う。
// 解析できない command を deny に倒すかどうかの条件で、引用の中の一致も拾うため誤って通す方向には働かない。
// commentlint:allow-long -- 解析前の広い一致が fail closed の条件である理由を残す
var branchPolicyGitCommand = regexp.MustCompile(`(?:^|[\s;&|('"` + "`" + `])(?:\S*/)?git` +
	`(?:\s+(?:-[cC]\s*(?:[^\s'"]*(?:'[^']*'|"[^"]*")|\S+)|-p|--paginate|--no-pager|--literal-pathspecs|--(?:git-dir|work-tree|exec-path)=\S+))*` +
	`\s+(?:checkout|switch|symbolic-ref)(?:[\s);&|'"` + "`" + `]|$)`)

// unsafeGitEnvironmentPrefixes は git の作業ツリーや git ディレクトリを差し替える環境変数の指定である。
var unsafeGitEnvironmentPrefixes = []string{"GIT_WORK_TREE=", "GIT_DIR=", "GIT_INDEX_FILE="}

// branchAttachHookOutput は wx session の Bash 呼び出しのうち、linked worktree へブランチを attach する git を deny する。
// main worktree と Git の管理下にないディレクトリでは何もしない。
// 対象の git 呼び出しを含むのに実行先か引数を静的に決められない command は、見逃しを防ぐため deny に倒す。
func branchAttachHookOutput(ctx context.Context, payload []byte) (preToolUseHookOutput, bool) {
	// 全ツール呼び出しで走るため、関係しない payload は JSON を解く前に落とす。
	raw := string(payload)
	if !strings.Contains(raw, "checkout") && !strings.Contains(raw, "switch") && !strings.Contains(raw, "symbolic-ref") {
		return preToolUseHookOutput{}, false
	}
	var decoded preToolUsePayload
	if json.Unmarshal(payload, &decoded) != nil {
		return preToolUseHookOutput{}, false
	}
	rawCommand, exists := decoded.ToolInput["command"]
	if !exists {
		return preToolUseHookOutput{}, false
	}
	var command string
	if json.Unmarshal(rawCommand, &command) != nil {
		return preToolUseHookOutput{}, false
	}
	cwd := decoded.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	runner := &gitx.Runner{Timeout: branchPolicyGitTimeout}
	switch classifyBranchAttach(ctx, runner, command, cwd) {
	case branchPolicyAttach:
		return denyPreToolUse(branchAttachReason, branchAttachSystemMessage), true
	case branchPolicyUnresolved:
		return denyPreToolUse(branchPolicyUnresolvedReason, branchPolicyUnresolvedSystemMessage), true
	case branchPolicyAllow:
	}
	return preToolUseHookOutput{}, false
}

type branchPolicyVerdict int

const (
	branchPolicyAllow branchPolicyVerdict = iota
	branchPolicyAttach
	branchPolicyUnresolved
)

// classifyBranchAttach は command 中の git 呼び出しを実行先ごとに判定する。
// 実行先が linked worktree である呼び出しだけを attach の判定にかけ、最初に見つかった違反を返す。
func classifyBranchAttach(ctx context.Context, runner *gitx.Runner, command, cwd string) branchPolicyVerdict {
	if !branchPolicyGitCommand.MatchString(command) {
		return branchPolicyAllow
	}
	tokens, wellFormed := lexPolicyCommand(command)
	if !wellFormed {
		return branchPolicyUnresolved
	}
	invocations, resolved := policyGitInvocations(tokens, cwd)
	if !resolved {
		return branchPolicyUnresolved
	}
	for _, invocation := range invocations {
		if !branchPolicySubcommands[invocation.subcommand] {
			continue
		}
		if invocation.target == "" {
			return branchPolicyUnresolved
		}
		if !isLinkedWorktree(ctx, runner, invocation.target) {
			continue
		}
		if invocation.dynamic {
			return branchPolicyUnresolved
		}
		var attaches, known bool
		switch invocation.subcommand {
		case "checkout":
			attaches, known = checkoutAttaches(ctx, runner, invocation.target, invocation.args)
		case "switch":
			attaches, known = switchAttaches(invocation.args), true
		default:
			attaches, known = symbolicRefAttaches(invocation.args), true
		}
		if !known {
			return branchPolicyUnresolved
		}
		if attaches {
			return branchPolicyAttach
		}
	}
	// 一次の一致が commit message・grep の pattern・echo の引数だった場合は、実際の subcommand ではないので通す。
	return branchPolicyAllow
}

// isLinkedWorktree は target が linked worktree の中かを git dir と共通 git dir の比較で判定する。
// rev-parse が失敗する場所（Git の管理下にないディレクトリなど）では対象の git 自身も失敗するので、linked ではないとして Git に任せる。
// commentlint:allow-long -- 失敗を linked でないとみなす根拠を残す
func isLinkedWorktree(ctx context.Context, runner *gitx.Runner, target string) bool {
	result, err := runner.Run(ctx, target, "rev-parse", "--path-format=absolute", "--absolute-git-dir", "--git-common-dir")
	if err != nil {
		return false
	}
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	if len(lines) != 2 {
		return false
	}
	gitDir, gitDirErr := filepath.EvalSymlinks(lines[0])
	commonDir, commonDirErr := filepath.EvalSymlinks(lines[1])
	if gitDirErr != nil || commonDirErr != nil {
		return false
	}
	return gitDir != commonDir
}

// checkoutAttaches は git checkout の引数がブランチを attach するかを返す。第 2 戻り値が false なら判定できない。
func checkoutAttaches(ctx context.Context, runner *gitx.Runner, target string, args []commandWord) (bool, bool) {
	for _, arg := range args {
		switch value := arg.value; {
		case value == "-b" || value == "-B" || value == "--orphan" || value == "--track" || value == "-t",
			strings.HasPrefix(value, "--orphan=") || strings.HasPrefix(value, "--track="):
			return true, true
		}
	}
	if hasWord(args, "--detach") {
		return false, true
	}
	// パスの操作は HEAD を動かさない。
	if hasWord(args, "--", "-p", "--patch", "--ours", "--theirs") {
		return false, true
	}
	if hasWord(args, "-") {
		return true, true
	}
	var candidate *commandWord
	for index := range args {
		if !strings.HasPrefix(args[index].value, "-") {
			candidate = &args[index]
			break
		}
	}
	if candidate == nil {
		return false, true
	}
	if !candidate.static() {
		return false, false
	}
	if strings.HasPrefix(candidate.value, "@{-") {
		return true, true
	}
	branch := strings.TrimPrefix(candidate.value, "refs/heads/")
	if _, err := runner.Run(ctx, target, "show-ref", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		return true, true
	}
	// `checkout foo` は origin/foo だけがある場合にも foo を作って attach する。
	remotes, err := runner.Run(ctx, target, "for-each-ref", "--format=%(refname:strip=3)", "refs/remotes")
	if err != nil {
		return false, false
	}
	return slices.Contains(strings.Split(remotes.Stdout, "\n"), candidate.value), true
}

func switchAttaches(args []commandWord) bool {
	for _, arg := range args {
		switch value := arg.value; {
		case value == "-c" || value == "-C" || value == "--create" || value == "--force-create",
			strings.HasPrefix(value, "--create=") || strings.HasPrefix(value, "--force-create="):
			return true
		}
	}
	if hasWord(args, "--detach", "-d") {
		return false
	}
	for _, arg := range args {
		if !strings.HasPrefix(arg.value, "-") || arg.value == "-" {
			return true
		}
	}
	return false
}

// symbolicRefAttaches は HEAD へ ref を書き込む形かを返す。-m と --reason の値は位置引数に数えない。
func symbolicRefAttaches(args []commandWord) bool {
	var positional []string
	skipValue := false
	for _, arg := range args {
		switch {
		case skipValue:
			skipValue = false
		case arg.value == "-m" || arg.value == "--reason":
			skipValue = true
		case !strings.HasPrefix(arg.value, "-"):
			positional = append(positional, arg.value)
		}
	}
	return len(positional) >= 2 && positional[0] == "HEAD"
}

func hasWord(args []commandWord, values ...string) bool {
	return slices.ContainsFunc(args, func(arg commandWord) bool { return slices.Contains(values, arg.value) })
}
