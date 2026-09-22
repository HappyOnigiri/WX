package agent

import (
	"os"
	"path/filepath"
	"strings"
)

// commandWord は引用を外した語と、静的な文字列として扱えるかの手掛かりを持つ。
type commandWord struct {
	value    string
	quoted   bool
	expanded bool
	globbed  bool
}

// static は語が実行時の展開を受けない文字列かを返す。
func (w commandWord) static() bool {
	return !w.expanded && !w.globbed
}

type policyTokenKind int

const (
	policyTokenWord policyTokenKind = iota
	policyTokenSeparator
	policyTokenOpen
	policyTokenClose
	// policyTokenRedirect の次の語はリダイレクト先で、コマンドの引数に数えない。
	policyTokenRedirect
)

type policyToken struct {
	kind policyTokenKind
	word commandWord
}

type pendingHeredoc struct {
	delimiter string
	stripTabs bool
	expands   bool
}

// policyLexer は lexPolicyCommand の走査状態である。
type policyLexer struct {
	command    string
	index      int
	tokens     []policyToken
	word       strings.Builder
	current    commandWord
	started    bool
	quote      byte
	heredocs   []pendingHeredoc
	wellFormed bool
	// heredocDelimiter は直前が `<<` か `<<-` で、次の語が heredoc の終端語であることを表す。
	heredocDelimiter, stripTabs bool
}

// lexPolicyCommand は command を語と shell の演算子の列にする。
// worktree add 用の lexShellCommand と違い、サブシェルの括弧・リダイレクト・heredoc の本文を区別する。
// 対象の git 呼び出しの見逃しを防ぐ側の近似なので、引用・heredoc が閉じないときは ok=false を返す。
// commentlint:allow-long -- lexShellCommand と分けた理由と、失敗の意味を残す
func lexPolicyCommand(command string) (tokens []policyToken, ok bool) {
	lexer := &policyLexer{command: command, wellFormed: true}
	for ; lexer.index < len(command); lexer.index++ {
		char := command[lexer.index]
		switch {
		case lexer.quote == '\'':
			lexer.singleQuoted(char)
		case lexer.quote == '"':
			lexer.doubleQuoted(char)
		case !lexer.operator(char):
			lexer.unquoted(char)
		}
	}
	lexer.flush()
	if lexer.quote != 0 || lexer.heredocDelimiter || len(lexer.heredocs) > 0 {
		lexer.wellFormed = false
	}
	return lexer.tokens, lexer.wellFormed
}

func (l *policyLexer) next() byte {
	if l.index+1 < len(l.command) {
		return l.command[l.index+1]
	}
	return 0
}

func (l *policyLexer) write(char byte) {
	l.word.WriteByte(char)
	l.started = true
}

func (l *policyLexer) flush() {
	if !l.started {
		return
	}
	l.current.value = l.word.String()
	if l.heredocDelimiter {
		l.heredocs = append(l.heredocs, pendingHeredoc{delimiter: l.current.value, stripTabs: l.stripTabs, expands: !l.current.quoted})
		l.heredocDelimiter = false
	}
	l.tokens = append(l.tokens, policyToken{kind: policyTokenWord, word: l.current})
	l.word.Reset()
	l.current = commandWord{}
	l.started = false
}

func (l *policyLexer) emit(kind policyTokenKind) {
	l.flush()
	l.tokens = append(l.tokens, policyToken{kind: kind})
}

func (l *policyLexer) singleQuoted(char byte) {
	if char == '\'' {
		l.quote = 0
		return
	}
	l.write(char)
}

func (l *policyLexer) doubleQuoted(char byte) {
	next := l.next()
	switch {
	case char == '"':
		l.quote = 0
	case char == '\\' && next != 0 && strings.IndexByte("$`\"\\\n", next) >= 0:
		l.index++
		if next != '\n' {
			l.write(next)
		}
	case char == '$' || char == '`':
		l.current.expanded = true
		l.write(char)
	default:
		l.write(char)
	}
}

// operator は引用の外の区切り・演算子・コメントを処理し、語の文字なら false を返す。
func (l *policyLexer) operator(char byte) bool {
	next := l.next()
	switch {
	case char == ' ' || char == '\t' || char == '\r':
		l.flush()
	case char == '#' && !l.started:
		for l.index+1 < len(l.command) && l.command[l.index+1] != '\n' {
			l.index++
		}
	case char == '\n':
		l.emit(policyTokenSeparator)
		var complete bool
		l.index, complete = skipHeredocBodies(l.command, l.index+1, l.heredocs)
		l.heredocs = nil
		l.wellFormed = l.wellFormed && complete
	case char == ';':
		l.emit(policyTokenSeparator)
	case char == '&' && next == '>':
		l.index++
		if l.next() == '>' {
			l.index++
		}
		l.emit(policyTokenRedirect)
	case char == '&' || char == '|':
		if next == char || (char == '|' && next == '&') {
			l.index++
		}
		l.emit(policyTokenSeparator)
	case char == '(':
		l.emit(policyTokenOpen)
	case char == ')':
		l.emit(policyTokenClose)
	case char == '<' || char == '>':
		l.redirect()
	default:
		return false
	}
	return true
}

func (l *policyLexer) redirect() {
	// `2>` の fd 番号は語ではなくリダイレクトの一部である。
	if l.started && !l.current.quoted && isDigits(l.word.String()) {
		l.word.Reset()
		l.current, l.started = commandWord{}, false
	}
	start := l.index
	l.index = consumeRedirect(l.command, l.index)
	switch l.command[start : l.index+1] {
	case "<<-":
		l.heredocDelimiter, l.stripTabs = true, true
	case "<<":
		l.heredocDelimiter, l.stripTabs = true, false
	}
	l.emit(policyTokenRedirect)
}

func (l *policyLexer) unquoted(char byte) {
	next := l.next()
	switch {
	case char == '\\':
		if next == 0 {
			l.wellFormed = false
			return
		}
		l.index++
		// 行末の `\` は行の継続で、語を作らない。
		if next != '\n' {
			l.write(next)
			l.current.quoted = true
		}
	case char == '\'' || char == '"':
		l.quote, l.started, l.current.quoted = char, true, true
	case char == '$' && next == '(':
		// `$(...)` の中身もコマンドとして読む。直前の語は展開を含むものとして閉じる。
		l.write(char)
		l.current.expanded = true
		l.emit(policyTokenOpen)
		l.index++
	case char == '$' || char == '`':
		l.write(char)
		l.current.expanded = true
	case char == '*' || char == '?' || char == '[':
		l.write(char)
		l.current.globbed = true
	default:
		l.write(char)
	}
}

// consumeRedirect は index にあるリダイレクト演算子の最後の文字の位置を返す。
func consumeRedirect(command string, index int) int {
	for _, operator := range []string{"<<<", "<<-", "<<", "<&", "<>", ">>", ">&", ">|", "<", ">"} {
		if strings.HasPrefix(command[index:], operator) {
			return index + len(operator) - 1
		}
	}
	return index
}

// skipHeredocBodies は start から始まる heredoc の本文を読み飛ばし、最後の本文の終端行の改行位置を返す。
// 展開を受ける本文にコマンド置換があると本文が実行されるので、読み飛ばさず complete=false を返す。
func skipHeredocBodies(command string, start int, heredocs []pendingHeredoc) (end int, complete bool) {
	end = start - 1
	for _, heredoc := range heredocs {
		found := false
		for end+1 < len(command) {
			lineStart := end + 1
			lineEnd := strings.IndexByte(command[lineStart:], '\n')
			if lineEnd < 0 {
				lineEnd = len(command)
			} else {
				lineEnd += lineStart
			}
			line := command[lineStart:lineEnd]
			end = lineEnd
			if heredoc.stripTabs {
				line = strings.TrimLeft(line, "\t")
			}
			if line == heredoc.delimiter {
				found = true
				break
			}
			if heredoc.expands && (strings.Contains(line, "$(") || strings.Contains(line, "`")) {
				return len(command), false
			}
		}
		if !found {
			return len(command), false
		}
	}
	return end, true
}

func isDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range []byte(value) {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

// policyGitInvocation は 1 つの git 呼び出しで、target が空なら実行先を静的に決められない。
type policyGitInvocation struct {
	subcommand string
	args       []commandWord
	target     string
	// dynamic は xargs や parallel が実行時に引数を足すことを表す。
	dynamic bool
}

// policyGitInvocations は cd とサブシェルを出現順に追い、git 呼び出しと各実行先を列挙する。
// 実行先や構造を静的に追えない形があれば resolved=false を返す。
func policyGitInvocations(tokens []policyToken, cwd string) (invocations []policyGitInvocation, resolved bool) {
	base := resolvePolicyDirectory(commandWord{value: cwd}, string(filepath.Separator))
	if base == "" {
		return nil, false
	}
	var stack []string
	var segment []commandWord
	redirectTarget := false
	process := func() bool {
		parts := segment
		segment = nil
		if redirectTarget {
			return false
		}
		invocation, isGit, ok := policySegment(parts, &base)
		if ok && isGit {
			invocations = append(invocations, invocation)
		}
		return ok
	}
	for _, token := range append(tokens, policyToken{kind: policyTokenSeparator}) {
		switch token.kind {
		case policyTokenWord:
			if redirectTarget {
				redirectTarget = false
				continue
			}
			if !token.word.quoted && (token.word.value == "{" || token.word.value == "}") {
				return nil, false
			}
			segment = append(segment, token.word)
		case policyTokenRedirect:
			if redirectTarget {
				return nil, false
			}
			redirectTarget = true
		case policyTokenOpen:
			if !process() {
				return nil, false
			}
			stack = append(stack, base)
		case policyTokenClose:
			if !process() || len(stack) == 0 {
				return nil, false
			}
			base = stack[len(stack)-1]
			stack = stack[:len(stack)-1]
		case policyTokenSeparator:
			if !process() {
				return nil, false
			}
		}
	}
	if len(stack) > 0 {
		return nil, false
	}
	return invocations, true
}

// policySegment は 1 つの simple command を読み、cd なら base を動かし、git なら呼び出しを返す。
func policySegment(parts []commandWord, base *string) (invocation policyGitInvocation, isGit, ok bool) {
	if len(parts) == 0 {
		return policyGitInvocation{}, false, true
	}
	for index, part := range parts {
		if !part.quoted && (part.value == "pushd" || part.value == "popd") {
			return policyGitInvocation{}, false, false
		}
		if nestedShellNames[filepath.Base(part.value)] && index+1 < len(parts) && strings.HasPrefix(parts[index+1].value, "-") {
			return policyGitInvocation{}, false, false
		}
		for _, prefix := range unsafeGitEnvironmentPrefixes {
			if strings.HasPrefix(part.value, prefix) {
				return policyGitInvocation{}, false, false
			}
		}
	}
	if parts[0].value == "cd" {
		valueIndex := 1
		if len(parts) > 1 && parts[1].value == "--" {
			valueIndex = 2
		}
		if valueIndex >= len(parts) || strings.HasPrefix(parts[valueIndex].value, "-") {
			return policyGitInvocation{}, false, false
		}
		if *base != "" {
			*base = resolvePolicyDirectory(parts[valueIndex], *base)
		}
		return policyGitInvocation{}, false, true
	}
	for index, part := range parts {
		if part.value != "git" && !strings.HasSuffix(part.value, "/git") {
			continue
		}
		invocation, ok := parsePolicyGitInvocation(parts[index+1:], *base)
		if !ok {
			return policyGitInvocation{}, false, false
		}
		for _, wrapper := range parts[:index] {
			if name := filepath.Base(wrapper.value); name == "xargs" || name == "parallel" {
				invocation.dynamic = true
			}
		}
		return invocation, true, true
	}
	return policyGitInvocation{}, false, true
}

// parsePolicyGitInvocation は git の大域オプションを読み、-C を重ねた実行先と subcommand を返す。
// 実行先だけが決められない場合も subcommand は返し、対象外の subcommand まで deny しないようにする。
// --git-dir と --work-tree は実行先の判定を無効にするので ok=false を返す。
func parsePolicyGitInvocation(args []commandWord, base string) (policyGitInvocation, bool) {
	target := base
	index := 0
	for index < len(args) {
		option := args[index].value
		switch {
		case option == "-C" || option == "-c":
			if index+1 >= len(args) {
				return policyGitInvocation{}, false
			}
			if option == "-C" && target != "" {
				target = resolvePolicyDirectory(args[index+1], target)
			}
			index += 2
			continue
		case strings.HasPrefix(option, "-C"):
			if target != "" {
				path := args[index]
				path.value = option[2:]
				target = resolvePolicyDirectory(path, target)
			}
		case strings.HasPrefix(option, "--git-dir") || strings.HasPrefix(option, "--work-tree"):
			return policyGitInvocation{}, false
		case !strings.HasPrefix(option, "-"):
			return policyGitInvocation{subcommand: option, args: args[index+1:], target: target}, true
		}
		index++
	}
	return policyGitInvocation{target: target}, true
}

// resolvePolicyDirectory は path を base から解決した実在ディレクトリの実パスを返し、解決できなければ空を返す。
// 先頭の ~ と ~/ だけを展開する。まだ作られていないパスは、実行時に cd が失敗した後の相対パスを取り違えないよう解決できないものとする。
// commentlint:allow-long -- 未作成のパスを解決不能とする理由を残す
func resolvePolicyDirectory(path commandWord, base string) string {
	value := path.value
	if !path.static() || value == "" {
		return ""
	}
	if value == "~" || strings.HasPrefix(value, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		value = home + value[1:]
	} else if strings.Contains(value, "~") {
		return ""
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(base, value)
	}
	resolved, err := filepath.EvalSymlinks(value)
	if err != nil {
		return ""
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return ""
	}
	return resolved
}
