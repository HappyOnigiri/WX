package agent

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
)

// preToolUsePayload は pre-tool-use payload のうち判定に使う部分だけを持つ。
// tool_input は未知フィールドを落とさずに書き戻すため、生の JSON のまま保持する。
// HookInput は比較可能である必要があるため（hook_fuzz_test.go の `!=`）、map を足さず型を分ける。
type preToolUsePayload struct {
	CWD       string                     `json:"cwd"`
	ToolInput map[string]json.RawMessage `json:"tool_input"`
}

// preToolUseHookSpecificOutput は Claude Code と Codex が共通で解釈する PreToolUse の応答である。
// updatedInput は文字列の command を持たないと Codex がエラーにするため、deny では省略する。
type preToolUseHookSpecificOutput struct {
	HookEventName            string                     `json:"hookEventName"`
	PermissionDecision       string                     `json:"permissionDecision"`
	PermissionDecisionReason string                     `json:"permissionDecisionReason"`
	AdditionalContext        string                     `json:"additionalContext,omitempty"`
	UpdatedInput             map[string]json.RawMessage `json:"updatedInput,omitempty"`
}

type preToolUseHookOutput struct {
	HookSpecificOutput preToolUseHookSpecificOutput `json:"hookSpecificOutput"`
	SystemMessage      string                       `json:"systemMessage,omitempty"`
}

const (
	rewriteSystemMessage = "wx rewrote git worktree add as wx new."
	denySystemMessage    = "wx blocked git worktree add; use wx new."
	rewriteReason        = "wx leases prepared worktrees, so git worktree add is replaced with wx new."
	denyGuidance         = "Run `wx new` (or `wx new --branch <branch>`) as its own tool call, then use the path it prints in the commands that follow."
)

// rewriteContext は書き換えを受けた agent が次に何をすべきかを伝える。
const rewriteContext = `wx replaced the command with wx new, which leases a workspace that wx prepares, tracks and returns.
- Use the path printed by wx new. wx chooses the path, so the path in the original command was never created.
- The leased workspace is detached. Run "git branch <name> HEAD" inside it when the work needs a branch.
- No need to run wx release: the lease is returned when this session ends.
- wx new can take a while when no prepared slot is standing by.`

const (
	denyReasonCompound        = "The command combines git worktree add with other commands, so wx cannot hand the leased path on to them."
	denyReasonOtherRepository = "git -C, --git-dir or --work-tree points the command at another repository."
	denyReasonBranchCreation  = "-b, -B and --orphan create a branch, while wx new leases a detached workspace."
	denyReasonStartPoint      = "The start point is not a branch name, and wx new --branch resolves only local branches and origin/<branch>."
	denyReasonUnsupported     = "wx cannot rewrite this form of git worktree add without guessing what it means."
)

type worktreeAddDecision int

const (
	worktreeAddIgnore worktreeAddDecision = iota
	worktreeAddRewrite
	worktreeAddDeny
)

type worktreeAddVerdict struct {
	decision worktreeAddDecision
	command  string
	reason   string
}

func ignoreWorktreeAdd() worktreeAddVerdict {
	return worktreeAddVerdict{decision: worktreeAddIgnore}
}

func denyWorktreeAdd(reason string) worktreeAddVerdict {
	return worktreeAddVerdict{decision: worktreeAddDeny, reason: reason}
}

// worktreeAddHookOutput は pre-tool-use payload を読み、単独の git worktree add を wx new へ写す。
// 候補を同定できない入力は fail-open で通し、候補を 1 つでも見つけた後は fail-closed で deny に倒す。
// hook は迂回できる UX であって強制境界ではないため、誤った deny の代償のほうが素通しより大きい。
// 第 2 戻り値が false のときは何も出力せず、従来どおり readiness 待ちだけで抜ける。
// commentlint:allow-long -- fail-open と fail-closed を切り替える根拠を残す
func worktreeAddHookOutput(payload []byte, workspaceRoot string) (preToolUseHookOutput, bool) {
	// 全ツール呼び出しで走るため、関係しない payload は JSON を解く前に落とす。
	if !strings.Contains(string(payload), "worktree") {
		return preToolUseHookOutput{}, false
	}
	var decoded preToolUsePayload
	if json.Unmarshal(payload, &decoded) != nil {
		return preToolUseHookOutput{}, false
	}
	raw, exists := decoded.ToolInput["command"]
	if !exists {
		return preToolUseHookOutput{}, false
	}
	var command string
	if json.Unmarshal(raw, &command) != nil {
		return preToolUseHookOutput{}, false
	}
	if !commandRunsInWorkspace(decoded.CWD, workspaceRoot) {
		return preToolUseHookOutput{}, false
	}
	verdict := classifyWorktreeAddCommand(command)
	switch verdict.decision {
	case worktreeAddRewrite:
		updated := make(map[string]json.RawMessage, len(decoded.ToolInput))
		maps.Copy(updated, decoded.ToolInput)
		encoded, err := json.Marshal(verdict.command)
		if err != nil {
			return preToolUseHookOutput{}, false
		}
		updated["command"] = encoded
		return preToolUseHookOutput{
			HookSpecificOutput: preToolUseHookSpecificOutput{
				HookEventName:            "PreToolUse",
				PermissionDecision:       "allow",
				PermissionDecisionReason: rewriteReason,
				AdditionalContext:        rewriteContext,
				UpdatedInput:             updated,
			},
			SystemMessage: rewriteSystemMessage,
		}, true
	case worktreeAddDeny:
		return preToolUseHookOutput{
			HookSpecificOutput: preToolUseHookSpecificOutput{
				HookEventName:            "PreToolUse",
				PermissionDecision:       "deny",
				PermissionDecisionReason: verdict.reason + " " + denyGuidance,
			},
			SystemMessage: denySystemMessage,
		}, true
	case worktreeAddIgnore:
		return preToolUseHookOutput{}, false
	}
	return preToolUseHookOutput{}, false
}

// writePreToolUseDecision は判定を 1 つの JSON として stdout へ書く。
// pre-tool-use の stdout は JSON として解釈されるため、判定が無いときは何も書かない。
func writePreToolUseDecision(payload []byte) {
	if len(payload) == 0 {
		return
	}
	output, ok := worktreeAddHookOutput(payload, os.Getenv("WX_WORKSPACE_ROOT"))
	if !ok {
		return
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(os.Stdout, string(encoded))
}

// commandRunsInWorkspace は cwd がこの session へ貸し出した workspace の配下かを判定する。
// 別の session の workspace で走る呼び出しを書き換えないための判定で、配下の入れ子 clone は区別しない。
// どの repository への git worktree add かはコマンド文字列からは決まらないためである。
func commandRunsInWorkspace(cwd, workspaceRoot string) bool {
	// cwd を載せない agent があるため、cwd が無いことだけを理由に判定を諦めない。
	if cwd == "" {
		return true
	}
	if workspaceRoot == "" {
		return false
	}
	root := filepath.Clean(workspaceRoot)
	target := filepath.Clean(cwd)
	return target == root || strings.HasPrefix(target, root+string(filepath.Separator))
}

// classifyWorktreeAddCommand は Bash の command 文字列を静的に読み、3 通りの判定を返す。
func classifyWorktreeAddCommand(command string) worktreeAddVerdict {
	segments, wellFormed := lexShellCommand(command)
	candidates := 0
	var chosen []shellWord
	var chosenIssue string
	var chosenExpanded bool
	for _, segment := range segments {
		arguments, issue, ok := gitWorktreeAddArguments(segment)
		if !ok {
			continue
		}
		candidates++
		chosen, chosenIssue = arguments, issue
		chosenExpanded = segmentExpands(segment)
	}
	if candidates == 0 {
		// 候補が見つからないときだけ、深さ 1 の `sh -c "..."` を拾う。eval や変数経由は塞がない。
		if nestedWorktreeAdd(segments) {
			return denyWorktreeAdd(denyReasonCompound)
		}
		return ignoreWorktreeAdd()
	}
	switch {
	case !wellFormed || chosenExpanded:
		return denyWorktreeAdd(denyReasonUnsupported)
	case candidates > 1 || len(segments) > 1:
		return denyWorktreeAdd(denyReasonCompound)
	case chosenIssue != "":
		return denyWorktreeAdd(chosenIssue)
	}
	return rewriteWorktreeAddArguments(chosen)
}

// worktreeAddDroppableFlags は wx new へ写すときに落としてよい git worktree add のフラグである。
// 未知の `-*` は deny に倒し、将来の git が足したフラグを黙って落とさない。
var worktreeAddDroppableFlags = map[string]bool{
	"-f": true, "--force": true, "--detach": true, "--checkout": true, "--no-checkout": true,
	"-q": true, "--quiet": true, "--no-track": true, "--guess-remote": true, "--no-guess-remote": true,
	"--relative-paths": true, "--no-relative-paths": true,
}

var worktreeAddBranchFlags = map[string]bool{"-b": true, "-B": true, "--orphan": true}

// rewriteWorktreeAddArguments は `add` 以降の引数を読み、wx new のコマンド行を組み立てる。
func rewriteWorktreeAddArguments(arguments []shellWord) worktreeAddVerdict {
	var positional []string
	positionalOnly := false
	for _, argument := range arguments {
		value := argument.value
		switch {
		case positionalOnly || !strings.HasPrefix(value, "-") || value == "-":
			positional = append(positional, value)
			continue
		case value == "--":
			positionalOnly = true
			continue
		case value == "-h" || value == "--help":
			// usage は git 自身に出させる。worktree は作られない。
			return ignoreWorktreeAdd()
		}
		name := value
		if index := strings.Index(name, "="); index > 0 {
			name = name[:index]
		}
		switch {
		case worktreeAddDroppableFlags[name]:
		case worktreeAddBranchFlags[name]:
			return denyWorktreeAdd(denyReasonBranchCreation)
		default:
			return denyWorktreeAdd(denyReasonUnsupported)
		}
	}
	if len(positional) == 0 || len(positional) > 2 {
		return denyWorktreeAdd(denyReasonUnsupported)
	}
	startPoint := ""
	if len(positional) == 2 {
		startPoint = positional[1]
	}
	command, ok := wxNewCommand(startPoint)
	if !ok {
		return denyWorktreeAdd(denyReasonStartPoint)
	}
	return worktreeAddVerdict{decision: worktreeAddRewrite, command: command}
}

// wxNewCommand は git worktree add の起点を wx new の呼び出しへ写す。
func wxNewCommand(startPoint string) (string, bool) {
	if startPoint == "" || startPoint == "HEAD" {
		return "wx new", true
	}
	branch := strings.TrimPrefix(startPoint, "origin/")
	if !isPlainBranchName(branch) {
		return "", false
	}
	return "wx new --branch " + branch, true
}

// isPlainBranchName は wx new --branch へそのまま渡せる名前かを判定する。
// 値は internal/pool を経て gitx.ResolveRef へ渡り、refs/heads と refs/remotes/origin しか引かない。
// SHA・tag・revision 式は必ず失敗し、`=` は repo=branch セレクタとして解釈されるため受け付けない。
// 受理する文字を限ることで、組み立てた wx new のコマンド行に shell の引用が要らなくなる。
// commentlint:allow-long -- 受理条件の由来（ResolveRef と repo セレクタ）を残す
func isPlainBranchName(name string) bool {
	if name == "" || strings.HasPrefix(name, "-") || strings.HasPrefix(name, "/") {
		return false
	}
	if strings.HasPrefix(name, "refs/") || strings.Contains(name, "..") || isHexObjectName(name) {
		return false
	}
	for _, char := range []byte(name) {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9':
		case char == '.' || char == '_' || char == '-' || char == '/' || char == '+':
		default:
			return false
		}
	}
	return true
}

func isHexObjectName(name string) bool {
	if len(name) < 7 || len(name) > 40 {
		return false
	}
	for _, char := range []byte(name) {
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') && !(char >= 'A' && char <= 'F') {
			return false
		}
	}
	return true
}

// gitRepositoryOptions は git 自身に別リポジトリを見せる大域オプションである。
var gitRepositoryOptions = map[string]bool{
	"-C": true, "--git-dir": true, "--work-tree": true, "--namespace": true,
}

// gitGlobalOptionsWithValue は次の語を値として取る大域オプションである。
var gitGlobalOptionsWithValue = map[string]bool{
	"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true, "--exec-path": true,
}

// gitWorktreeAddArguments は segment が git worktree add かを判定し、`add` 以降の引数を返す。
// argv[0] が git であることを必ず条件にするので、`echo "git worktree add"` や grep は通過する。
// issue は候補ではあるが書き換えられない理由で、空でなければ呼び出し側が deny に使う。
func gitWorktreeAddArguments(segment []shellWord) (arguments []shellWord, issue string, ok bool) {
	index := 0
	// 先頭の `NAME=value` は環境変数の指定で、git の参照先を差し替え得る。
	for index < len(segment) && isAssignmentWord(segment[index].value) {
		index++
		issue = denyReasonOtherRepository
	}
	if index >= len(segment) || filepath.Base(segment[index].value) != "git" {
		return nil, "", false
	}
	index++
	for index < len(segment) && strings.HasPrefix(segment[index].value, "-") {
		name := segment[index].value
		if position := strings.Index(name, "="); position > 0 {
			name = name[:position]
		} else if gitGlobalOptionsWithValue[name] {
			index++
		}
		if issue == "" {
			issue = denyReasonUnsupported
			if gitRepositoryOptions[name] {
				issue = denyReasonOtherRepository
			}
		}
		index++
	}
	if index+1 >= len(segment) || segment[index].value != "worktree" || segment[index+1].value != "add" {
		return nil, "", false
	}
	return segment[index+2:], issue, true
}

func isAssignmentWord(word string) bool {
	index := strings.Index(word, "=")
	if index <= 0 {
		return false
	}
	for _, char := range []byte(word[:index]) {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9', char == '_':
		default:
			return false
		}
	}
	return true
}

var nestedShellNames = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true}

// nestedWorktreeAdd は `sh -c "git worktree add ..."` を深さ 1 だけ見る。
func nestedWorktreeAdd(segments [][]shellWord) bool {
	for _, segment := range segments {
		if len(segment) == 0 || !nestedShellNames[filepath.Base(segment[0].value)] {
			continue
		}
		for index := 1; index+1 < len(segment); index++ {
			if segment[index].value != "-c" {
				continue
			}
			inner, _ := lexShellCommand(segment[index+1].value)
			for _, innerSegment := range inner {
				if _, _, ok := gitWorktreeAddArguments(innerSegment); ok {
					return true
				}
			}
		}
	}
	return false
}

// shellWord は 1 つの語と、その語が変数展開・コマンド置換を含むかを持つ。
type shellWord struct {
	value    string
	expanded bool
}

func segmentExpands(segment []shellWord) bool {
	for _, word := range segment {
		if word.expanded {
			return true
		}
	}
	return false
}

// shellSeparators は segment を切る shell の演算子とリダイレクトである。
const shellSeparators = ";&|\n\r()<>"

// lexShellCommand は command を shell の演算子で segment へ分け、各 segment を語へ分解する。
// 引用が閉じない場合も途中までの語を返して ok=false とし、候補を見つけた側が deny に倒せるようにする。
// 厳密な shell の解釈ではなく、単一の simple command かどうかを見分けるのに足りる近似である。
// 最初の `<<` で読むのをやめ、heredoc の本文を語彙解析へ入れない。
// 終端語を同定しないのは、引用形・`<<-`・複数 heredoc・未終端を取り違えると
// 本文の範囲が伸び縮みし、近似の穴が増えるためである。候補が減る方向にしか効かないので、
// 本文の後ろに書いた `git worktree add` は素通しになる（`<<<` も本文とみなす）。
// commentlint:allow-long -- 終端語を見ない選択と、その代償である素通しを残す
func lexShellCommand(command string) (segments [][]shellWord, ok bool) {
	var segment []shellWord
	var word strings.Builder
	var quote byte
	started, expanded, escaped := false, false, false
	wellFormed := true
	flushWord := func() {
		if started {
			segment = append(segment, shellWord{value: word.String(), expanded: expanded})
			word.Reset()
			started, expanded = false, false
		}
	}
	flushSegment := func() {
		flushWord()
		if len(segment) > 0 {
			segments = append(segments, segment)
			segment = nil
		}
	}
scan:
	for index := 0; index < len(command); index++ {
		char := command[index]
		switch {
		case escaped:
			word.WriteByte(char)
			escaped, started = false, true
		case quote != 0:
			switch {
			case char == quote:
				quote = 0
				started = true
			case quote == '"' && char == '\\':
				escaped = true
			case quote == '"' && (char == '$' || char == '`'):
				expanded, started = true, true
				word.WriteByte(char)
			default:
				word.WriteByte(char)
				started = true
			}
		case char == '\\':
			escaped, started = true, true
		case char == '\'' || char == '"':
			quote, started = char, true
		case char == ' ' || char == '\t':
			flushWord()
		case char == '<' && index+1 < len(command) && command[index+1] == '<':
			break scan
		case strings.IndexByte(shellSeparators, char) >= 0:
			flushSegment()
		case char == '$' || char == '`':
			expanded, started = true, true
			word.WriteByte(char)
		default:
			word.WriteByte(char)
			started = true
		}
	}
	if quote != 0 || escaped {
		wellFormed = false
	}
	flushSegment()
	return segments, wellFormed
}
