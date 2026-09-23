package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// commandWord は引用を外した語と、静的な文字列として扱えるかの手掛かりを持つ。
type commandWord struct {
	value    string
	quoted   bool
	expanded bool
	globbed  bool
	// tilde は語が引用されていない ~ で始まり、shell がホームへ展開し得ることを表す。
	tilde bool
	// substitutions は語の中の二重引用符内の `$(...)` とバッククオートの本文で、語とは別のサブシェルとして判定する。
	// commentlint:allow-long -- 本文を語に残したまま別に判定する理由を残す
	substitutions [][]policyToken
}

// static は語が実行時の展開を受けない文字列かを返す。
func (w commandWord) static() bool {
	return !w.expanded && !w.globbed
}

type policyTokenKind int

const (
	policyTokenWord policyTokenKind = iota
	policyTokenSeparator
	// policyTokenPipe と policyTokenBackground は区切りのうち、左の command を別プロセスで走らせ得るものである。
	policyTokenPipe
	policyTokenBackground
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
	// depth は開いている括弧の数で、置換の本文の終端 `)` を入れ子の `)` と区別する。
	depth int
}

// lexPolicyCommand は command を語と shell の演算子の列にする。
// worktree add 用の lexShellCommand と違い、サブシェルの括弧・リダイレクト・heredoc の本文を区別する。
// 対象の git 呼び出しの見逃しを防ぐ側の近似なので、引用・heredoc が閉じないときは ok=false を返す。
// commentlint:allow-long -- lexShellCommand と分けた理由と、失敗の意味を残す
func lexPolicyCommand(command string) (tokens []policyToken, ok bool) {
	tokens, _, ok = lexPolicySpan(command, 0, 0)
	return tokens, ok
}

// lexPolicySpan は start から字句解析する。closer が 0 でなければ、引用と括弧の外に現れた closer の位置で止まり、その位置を end に返す。
// closer が現れないまま終わった場合は ok=false を返す。
// commentlint:allow-long -- 置換の本文を同じ字句解析で読む契約を残す
func lexPolicySpan(command string, start int, closer byte) (tokens []policyToken, end int, ok bool) {
	lexer := &policyLexer{command: command, index: start, wellFormed: true}
	closed := closer == 0
	for ; lexer.index < len(command); lexer.index++ {
		char := command[lexer.index]
		if closer != 0 && lexer.quote == 0 && char == closer && lexer.depth == 0 {
			closed = true
			break
		}
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
	if !closed || lexer.quote != 0 || lexer.heredocDelimiter || len(lexer.heredocs) > 0 {
		lexer.wellFormed = false
	}
	return lexer.tokens, lexer.index, lexer.wellFormed
}

// substitution は index にある `$(` かバッククオートから始まる置換を読み、本文の token を語に付ける。
// 語には置換の原文を残し、展開を含む印を付ける。本文が閉じなければ command 全体を解析できないものとする。
// commentlint:allow-long -- 語の値と本文の判定を分けて持つ理由を残す
func (l *policyLexer) substitution(bodyStart int, closer byte) {
	start := l.index
	inner, end, ok := lexPolicySpan(l.command, bodyStart, closer)
	if !ok {
		l.wellFormed = false
		l.index = len(l.command)
		return
	}
	for _, char := range []byte(l.command[start : end+1]) {
		l.write(char)
	}
	l.current.expanded = true
	l.current.substitutions = append(l.current.substitutions, inner)
	l.index = end
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
	case char == '$' && next == '(':
		l.substitution(l.index+2, ')')
	case char == '`':
		l.substitution(l.index+1, '`')
	case char == '$':
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
	case next == char && (char == '&' || char == '|'):
		l.index++
		l.emit(policyTokenSeparator)
	case char == '|':
		if next == '&' {
			l.index++
		}
		l.emit(policyTokenPipe)
	case char == '&':
		l.emit(policyTokenBackground)
	case char == '(':
		l.depth++
		l.emit(policyTokenOpen)
	case char == ')':
		l.depth--
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
		l.depth++
		l.emit(policyTokenOpen)
		l.index++
	case char == '`':
		l.substitution(l.index+1, '`')
	case char == '$':
		l.write(char)
		l.current.expanded = true
	case char == '*' || char == '?' || char == '[':
		l.write(char)
		l.current.globbed = true
	case char == '~' && !l.started:
		l.write(char)
		l.current.tilde = true
	case char == '{' && isBraceExpansion(l.command[l.index+1:]):
		// `{a,b}` と `{1..3}` は shell が別の語に展開するので、静的な文字列として扱わない。
		l.write(char)
		l.current.globbed = true
	default:
		l.write(char)
	}
}

// isBraceExpansion は `{` の直後から、語が終わる前に `}` で閉じ、その間に `,` か `..` があるかを返す。
// find -exec の `{}` のように区切りのない括弧は展開されないので false を返す。
func isBraceExpansion(rest string) bool {
	end := strings.IndexAny(rest, "} \t\r\n;&|()<>")
	if end < 0 || rest[end] != '}' {
		return false
	}
	body := rest[:end]
	return strings.Contains(body, ",") || strings.Contains(body, "..")
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

// policyFrame はサブシェルに入る前の状態で、閉じ括弧で戻す。segment はプロセス置換の外の command の読みかけである。
type policyFrame struct {
	base    string
	segment []commandWord
	piped   bool
}

// policyGitInvocations は cd とサブシェルを出現順に追い、git 呼び出しと各実行先を列挙する。
// 実行先や構造を静的に追えない形があれば resolved=false を返す。
func policyGitInvocations(tokens []policyToken, cwd string) (invocations []policyGitInvocation, resolved bool) {
	base := resolvePolicyDirectory(commandWord{value: cwd}, string(filepath.Separator))
	if base == "" {
		return nil, false
	}
	return policyGitInvocationsFrom(tokens, base)
}

// policyGitInvocationsFrom は解決済みの base から tokens を追う。base が空なら実行先を決められない呼び出しとして返す。
func policyGitInvocationsFrom(tokens []policyToken, base string) (invocations []policyGitInvocation, resolved bool) {
	var stack []policyFrame
	var segment []commandWord
	redirectTarget := false
	// piped は直前の区切りがパイプで、いま読んでいる command がパイプラインの一部であることを表す。
	piped := false
	// process は区切り next の手前までの command を読む。
	// `&` の左の command は別プロセスで走るので、その cd を後続へ引き継がない。
	// パイプラインの要素は shell によって別プロセスかどうかが違うので、cd があれば以降の実行先を決められないものとする。
	// commentlint:allow-long -- 区切りの種類で cd の引き継ぎを変える理由を残す
	process := func(next policyTokenKind) bool {
		parts := segment
		segment = nil
		if redirectTarget {
			return false
		}
		previous := base
		invocation, isGit, ok := policySegment(parts, &base)
		if ok && isGit {
			invocations = append(invocations, invocation)
		}
		switch {
		case next == policyTokenBackground:
			base = previous
		case (next == policyTokenPipe || piped) && base != previous:
			base = ""
		}
		piped = next == policyTokenPipe
		return ok
	}
	all := append(append([]policyToken(nil), tokens...), policyToken{kind: policyTokenSeparator})
	for index, token := range all {
		switch token.kind {
		case policyTokenWord:
			// 置換の本文はその時点の実行先で走るサブシェルで、中の cd は外へ持ち出さない。
			for _, inner := range token.word.substitutions {
				nested, ok := policyGitInvocationsFrom(inner, base)
				if !ok {
					return nil, false
				}
				invocations = append(invocations, nested...)
			}
			if redirectTarget {
				redirectTarget = false
				continue
			}
			if !token.word.quoted && (token.word.value == "{" || token.word.value == "}") {
				return nil, false
			}
			segment = append(segment, token.word)
		case policyTokenRedirect:
			// `> >(...)` の 2 つ目の `>` はプロセス置換の一部で、1 つ目のリダイレクト先になる。
			if redirectTarget && all[index+1].kind != policyTokenOpen {
				return nil, false
			}
			redirectTarget = true
		case policyTokenOpen:
			if redirectTarget {
				// `<(...)` と `>(...)` はプロセス置換で、本文をサブシェルとして読んだ後に外の command の続きへ戻る。
				// commentlint:allow-long -- 外の command を保存したまま本文を読む理由を残す
				redirectTarget = false
				stack = append(stack, policyFrame{base: base, segment: segment, piped: piped})
				segment, piped = nil, false
				continue
			}
			if !process(token.kind) {
				return nil, false
			}
			stack = append(stack, policyFrame{base: base})
		case policyTokenClose:
			if !process(token.kind) || len(stack) == 0 {
				return nil, false
			}
			frame := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			base, segment, piped = frame.base, frame.segment, frame.piped
		case policyTokenSeparator, policyTokenPipe, policyTokenBackground:
			if !process(token.kind) {
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
	commandIndex := 0
	for commandIndex < len(parts) && !parts[commandIndex].quoted && isAssignmentWord(parts[commandIndex].value) {
		commandIndex++
	}
	if commandIndex < len(parts) {
		command := parts[commandIndex]
		switch name := filepath.Base(command.value); {
		case !command.quoted && command.value == "eval":
			// eval は引数を連結して実行時にコマンドとして読むので、静的には追えない。
			return policyGitInvocation{}, false, false
		case nestedShellNames[name] && !slices.ContainsFunc(parts[commandIndex+1:], func(arg commandWord) bool { return !strings.HasPrefix(arg.value, "-") }):
			// 引数の無い shell は stdin・heredoc・here-string からコマンドを読む。
			return policyGitInvocation{}, false, false
		}
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
			switch filepath.Base(wrapper.value) {
			// find の -exec 系も xargs と同じく、見つかったパスを実行時に引数へ足す。
			case "xargs", "parallel", "-exec", "-execdir", "-ok", "-okdir":
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
		case option == "--namespace" || option == "--config-env":
			// 次の語は値で、subcommand ではない。
			index++
		case !strings.HasPrefix(option, "-"):
			return policyGitInvocation{subcommand: option, args: args[index+1:], target: target}, true
		}
		index++
	}
	return policyGitInvocation{target: target}, true
}

// resolvePolicyDirectory は path を base から解決した実在ディレクトリの実パスを返し、解決できなければ空を返す。
// 引用されていない先頭の ~ と ~/ だけを展開し、引用された ~ は文字どおりのパスとして扱う。
// まだ作られていないパスは、実行時に cd が失敗した後の相対パスを取り違えないよう解決できないものとする。
// commentlint:allow-long -- 未作成のパスを解決不能とする理由を残す
func resolvePolicyDirectory(path commandWord, base string) string {
	value := path.value
	if !path.static() || value == "" {
		return ""
	}
	switch {
	case path.tilde && (value == "~" || strings.HasPrefix(value, "~/")):
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		value = home + value[1:]
	case path.tilde || (!path.quoted && strings.Contains(value, "~")):
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
