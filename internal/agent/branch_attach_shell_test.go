package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// policyTokenString は token 列を比較しやすい文字列にする。語は値を、演算子は種類の記号を並べる。
func policyTokenString(tokens []policyToken) string {
	parts := make([]string, 0, len(tokens))
	for _, token := range tokens {
		switch token.kind {
		case policyTokenWord:
			parts = append(parts, token.word.value)
		case policyTokenSeparator:
			parts = append(parts, ";")
		case policyTokenPipe:
			parts = append(parts, "|")
		case policyTokenBackground:
			parts = append(parts, "&")
		case policyTokenOpen:
			parts = append(parts, "(")
		case policyTokenClose:
			parts = append(parts, ")")
		case policyTokenRedirect:
			parts = append(parts, ">")
		}
	}
	return strings.Join(parts, " ")
}

func TestLexPolicyCommandKeepsCommandAfterComment(t *testing.T) {
	command := strings.Join([]string{"git", "status", "# ignore\ngit", "switch", "-c", "feature"}, " ")
	tokens, ok := lexPolicyCommand(command)
	if !ok {
		t.Fatal("command was not well formed")
	}
	want := strings.Join([]string{"git", "status", ";", "git", "switch", "-c", "feature"}, " ")
	if got := policyTokenString(tokens); got != want {
		t.Fatalf("tokens=%q, want %q", got, want)
	}
}

func TestLexPolicyCommandTracksNestedQuotedSubstitution(t *testing.T) {
	inner := "$" + "(" + "printf y" + ")"
	outer := "$" + "(" + "printf x " + inner + ")"
	command := `echo "` + outer + `"`
	if _, ok := lexPolicyCommand(command); !ok {
		t.Fatal("nested command substitution was not well formed")
	}
}

func TestPolicySegmentRejectsWrappedShellWithoutCommand(t *testing.T) {
	base := t.TempDir()
	parts := []commandWord{{value: "su" + "do"}, {value: "ba" + "sh"}}
	if _, isGit, ok := policySegment(parts, &base); ok || isGit {
		t.Fatal("a wrapped shell without a command was resolved")
	}
}

func TestIsDigitsIncludesASCIIEndpoints(t *testing.T) {
	for value, want := range map[string]bool{
		"0":  true,
		"9":  true,
		"/":  false,
		":":  false,
		"09": true,
	} {
		if got := isDigits(value); got != want {
			t.Errorf("isDigits(%q)=%v, want %v", value, got, want)
		}
	}
}

func TestLexPolicyCommandRejectsMissingEmptyHeredocTerminator(t *testing.T) {
	if _, ok := lexPolicyCommand("cat <<''\nbody\n"); ok {
		t.Fatal("heredoc without its empty terminator was accepted")
	}
}

func TestLexPolicyCommandStructure(t *testing.T) {
	tests := []struct {
		command, want string
		ok            bool
	}{
		{command: "git switch other", want: "git switch other", ok: true},
		{command: "a && b || c | d & e; f\ng", want: "a ; b ; c | d & e ; f ; g", ok: true},
		{command: "a |& b", want: "a | b", ok: true},
		{command: "(cd x && git status)", want: "( cd x ; git status )", ok: true},
		{command: "echo $(git switch x)", want: "echo $ ( git switch x )", ok: true},
		{command: `git commit -m "a $(b) c"`, want: "git commit -m a $(b) c", ok: true},
		{command: `echo 'it''s' "q\"d" e\ f`, want: `echo its q"d e f`, ok: true},
		{command: "git \\\n  switch x", want: "git switch x", ok: true},
		// fd 番号はリダイレクトの一部で、次の語はリダイレクト先である。
		{command: "git switch x 2>/dev/null", want: "git switch x > /dev/null", ok: true},
		{command: "git switch x >log 2>&1", want: "git switch x > log > 1", ok: true},
		{command: "git switch x &>log", want: "git switch x > log", ok: true},
		{command: "echo '2'>log", want: "echo 2 > log", ok: true},
		{command: "git switch x # note (unbalanced", want: "git switch x", ok: true},
		{command: "echo a#b", want: "echo a#b", ok: true},
		{command: "cat <<'EOF'\ngit switch x\nEOF\ngit status", want: "cat > EOF ; git status", ok: true},
		{command: "cat <<A <<B\na\nA\nb\nB\ngit status", want: "cat > A > B ; git status", ok: true},
		{command: "cat <<-EOF\n\tbody\n\tEOF", want: "cat > EOF ;", ok: true},
		{command: "cat <<EOF\n$HOME\nEOF", want: "cat > EOF ;", ok: true},
		{command: "grep x <<< 'git switch x'", want: "grep x > git switch x", ok: true},
		{command: "git switch 'x", want: "git switch x", ok: false},
		{command: "git switch x\\", want: "git switch x", ok: false},
		{command: "cat <<EOF\nbody", want: "cat > EOF ;", ok: false},
		{command: "cat <<EOF", want: "cat > EOF", ok: false},
		{command: "cat <<'EOF'\n`date`\nEOF", want: "cat > EOF ;", ok: true},
		{command: "cat <<EOF\n`date`\nEOF", want: "cat > EOF ;", ok: false},
	}
	for _, test := range tests {
		t.Run(test.command, func(t *testing.T) {
			tokens, ok := lexPolicyCommand(test.command)
			if got := policyTokenString(tokens); got != test.want || ok != test.ok {
				t.Fatalf("lex(%q)=%q ok=%v, want %q ok=%v", test.command, got, ok, test.want, test.ok)
			}
		})
	}
}

func TestLexPolicyCommandMarksDynamicWords(t *testing.T) {
	tokens, ok := lexPolicyCommand(`git checkout $A "$B" 'lit$C' * {} { ~/x "~" a~ {a,b} x{1..3} '{a,b}' {a}`)
	if !ok {
		t.Fatal("command was not well formed")
	}
	var got []commandWord
	for _, token := range tokens {
		got = append(got, token.word)
	}
	want := []commandWord{
		{value: "git"},
		{value: "checkout"},
		{value: "$A", expanded: true},
		{value: "$B", quoted: true, expanded: true},
		{value: "lit$C", quoted: true},
		{value: "*", globbed: true},
		{value: "{}"},
		{value: "{"},
		{value: "~/x", tilde: true},
		{value: "~", quoted: true},
		{value: "a~"},
		{value: "{a,b}", globbed: true},
		{value: "x{1..3}", globbed: true},
		{value: "{a,b}", quoted: true},
		{value: "{a}"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("words=%+v, want %+v", got, want)
	}
}

func TestPolicyGitInvocationsTrackDirectories(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		command string
		targets []string
	}{
		{command: "git status", targets: []string{root}},
		{command: "cd child && git status; git log", targets: []string{child, child}},
		{command: "(cd child && git status) && git log", targets: []string{child, root}},
		{command: "git -C child status", targets: []string{child}},
		{command: "git -C child -C .. status", targets: []string{root}},
		{command: "cd missing && git status", targets: []string{""}},
		{command: "cd missing && cd " + child + " && git status", targets: []string{""}},
		// `&` の左の cd は別プロセスで走り、パイプラインの cd は shell によって引き継ぎが違う。
		{command: "cd child & git status", targets: []string{root}},
		{command: "cd child | git status; git log", targets: []string{"", ""}},
		{command: "echo | cd child; git status", targets: []string{""}},
		{command: "git status | cat && git log", targets: []string{root, root}},
		{command: "git -c x.y=z status", targets: []string{root}},
		{command: "diff <(cd child && git show) x; git log", targets: []string{child, root}},
		{command: "git diff > >(cd child && git apply) && git log", targets: []string{child, root, root}},
		{command: `echo "{" && git status`, targets: []string{root}},
		{command: "git", targets: []string{root}},
	}
	for _, test := range tests {
		t.Run(test.command, func(t *testing.T) {
			tokens, ok := lexPolicyCommand(test.command)
			if !ok {
				t.Fatalf("lex(%q) failed", test.command)
			}
			invocations, resolved := policyGitInvocations(tokens, root)
			if !resolved {
				t.Fatalf("invocations for %q were not resolved", test.command)
			}
			var targets []string
			for _, invocation := range invocations {
				targets = append(targets, invocation.target)
			}
			if !reflect.DeepEqual(targets, test.targets) {
				t.Fatalf("targets=%q, want %q", targets, test.targets)
			}
		})
	}
	for _, command := range []string{"git -C", "git -c", "echo >", "echo > ;", "cd -P child"} {
		tokens, _ := lexPolicyCommand(command)
		if _, resolved := policyGitInvocations(tokens, root); resolved {
			t.Fatalf("invocations for %q were resolved", command)
		}
	}
	if _, resolved := policyGitInvocations(nil, filepath.Join(root, "missing")); resolved {
		t.Fatal("a missing cwd was resolved")
	}
}

func TestResolvePolicyDirectoryExpandsOnlyLeadingTilde(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	if err := os.Mkdir(filepath.Join(home, "repo"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, "~"), 0o700); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		path commandWord
		want string
	}{
		{path: commandWord{value: "~", tilde: true}, want: home},
		{path: commandWord{value: "~/repo", tilde: true}, want: filepath.Join(home, "repo")},
		{path: commandWord{value: "repo"}, want: filepath.Join(home, "repo")},
		{path: commandWord{value: "~other/repo", tilde: true}, want: ""},
		// 引用された ~ は展開されず、base からの相対パスになる。
		{path: commandWord{value: "~", quoted: true}, want: filepath.Join(home, "~")},
		{path: commandWord{value: "~/repo", quoted: true}, want: ""},
		{path: commandWord{value: "repo~"}, want: ""},
		{path: commandWord{value: "file"}, want: ""},
		{path: commandWord{value: ""}, want: ""},
		{path: commandWord{value: "repo", expanded: true}, want: ""},
		{path: commandWord{value: "rep?", globbed: true}, want: ""},
	}
	for _, test := range tests {
		if got := resolvePolicyDirectory(test.path, home); got != test.want {
			t.Fatalf("resolve(%+v)=%q, want %q", test.path, got, test.want)
		}
	}
}
