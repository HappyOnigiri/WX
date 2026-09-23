package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/gitx"
)

// branchPolicyFixture は main worktree と、detached・attached の linked worktree を持つ本物のリポジトリである。
type branchPolicyFixture struct {
	tmp, main, detached, attached string
}

// newBranchPolicyFixture は git の設定を GIT_CONFIG_GLOBAL=/dev/null で隔離して作る。
// 隔離しないと core.hooksPath によって実環境の git hook が worktree add で走る。
func newBranchPolicyFixture(t *testing.T) branchPolicyFixture {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fixture := branchPolicyFixture{
		tmp:      tmp,
		main:     filepath.Join(tmp, "main"),
		detached: filepath.Join(tmp, "detached"),
		attached: filepath.Join(tmp, "attached"),
	}
	if err := os.Mkdir(fixture.main, 0o700); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, fixture.main, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(fixture.main, "tracked.txt"), []byte("tracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, fixture.main, "add", "tracked.txt")
	fixtureGit(t, fixture.main, "commit", "-q", "-m", "initial")
	fixtureGit(t, fixture.main, "branch", "other")
	fixtureGit(t, fixture.main, "worktree", "add", "-q", "--detach", fixture.detached, "HEAD")
	fixtureGit(t, fixture.main, "worktree", "add", "-q", "-b", "legacy-attached", fixture.attached)
	fixtureGit(t, fixture.main, "update-ref", "refs/remotes/origin/remote-only", "HEAD")
	// 通常の clone と同じく origin/HEAD を置き、remote の一覧に HEAD が現れる状態で判定する。
	fixtureGit(t, fixture.main, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/remote-only")
	return fixture
}

func fixtureGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	var environment []string
	for _, entry := range os.Environ() {
		// 実行中の git hook から継承した GIT_DIR などが、fixture 以外のリポジトリを指さないようにする。
		if !strings.HasPrefix(entry, "GIT_") {
			environment = append(environment, entry)
		}
	}
	environment = append(environment,
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=wx", "GIT_AUTHOR_EMAIL=wx@example.invalid",
		"GIT_COMMITTER_NAME=wx", "GIT_COMMITTER_EMAIL=wx@example.invalid",
	)
	command.Env = environment
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func classifyInFixture(command, cwd string) branchPolicyVerdict {
	return classifyBranchAttach(context.Background(), &gitx.Runner{Timeout: branchPolicyGitTimeout}, command, cwd)
}

type branchPolicyCase struct {
	command, cwd string
	want         branchPolicyVerdict
}

func runBranchPolicyCases(t *testing.T, cases []branchPolicyCase) {
	t.Helper()
	for _, test := range cases {
		t.Run(test.command, func(t *testing.T) {
			if got := classifyInFixture(test.command, test.cwd); got != test.want {
				t.Fatalf("classify(%q in %s)=%d, want %d", test.command, test.cwd, got, test.want)
			}
		})
	}
}

func TestClassifyBranchAttachInLinkedWorktree(t *testing.T) {
	fixture := newBranchPolicyFixture(t)
	var cases []branchPolicyCase
	for _, command := range []string{
		"git checkout other",
		"git checkout -b new-branch",
		"git checkout -b new-branch --detach",
		"git checkout --track origin/remote-only",
		// origin/remote-only から remote-only を作って attach する。
		"git checkout remote-only",
		"git checkout -",
		"git checkout @{-1}",
		"git checkout refs/heads/other",
		"git switch other",
		"git switch -",
		"git switch -c new-branch",
		"git switch -c new-branch --detach",
		"git switch --create=new-branch",
		"git checkout -bnew-branch",
		"git checkout -Bnew-branch",
		"git switch -cnew-branch",
		"git switch -Cnew-branch",
		"git symbolic-ref HEAD refs/heads/other",
		"git symbolic-ref -m reason HEAD refs/heads/other",
		"git -c core.quotepath=false switch other",
		"/usr/bin/git switch other",
		// 一次の正規表現が拾わない大域オプションや引用も、解析で git 呼び出しとして読む。
		"git -P switch other",
		"git --no-optional-locks switch other",
		"git --config-env core.editor=EDITOR switch other",
		`"git" switch other`,
		`git "switch" other`,
		`\git switch other`,
		"git checkout other 2>/dev/null",
		"git checkout other >/dev/null 2>&1",
		"git switch other # back to other",
		"git status && git switch other",
		"echo $(git checkout other)",
		// 二重引用符内とバッククオートの置換の本文も実行される。
		`x="$(git switch other 2>&1)"`,
		`echo "$(git checkout other)"`,
		"echo `git checkout other`",
		`echo "$(cd .. && echo "$(git -C detached switch other)")"`,
		"git fetch origin\ngit switch other",
	} {
		cases = append(cases, branchPolicyCase{command: command, cwd: fixture.detached, want: branchPolicyAttach})
	}
	cases = append(cases,
		branchPolicyCase{command: "cd " + fixture.detached + " && git switch other", cwd: fixture.main, want: branchPolicyAttach},
		branchPolicyCase{command: "cd -- " + fixture.detached + " && git switch other", cwd: fixture.main, want: branchPolicyAttach},
		branchPolicyCase{command: "git -C " + fixture.detached + " checkout other", cwd: fixture.main, want: branchPolicyAttach},
		branchPolicyCase{command: "git -C" + fixture.detached + " checkout other", cwd: fixture.main, want: branchPolicyAttach},
		branchPolicyCase{command: "git -C " + fixture.tmp + " -C detached checkout other", cwd: fixture.main, want: branchPolicyAttach},
		branchPolicyCase{command: "cd ../detached && git switch other", cwd: fixture.main, want: branchPolicyAttach},
		branchPolicyCase{command: "(cd " + fixture.main + " && git status) && git switch other", cwd: fixture.detached, want: branchPolicyAttach},
		// バックグラウンドの cd は後続の実行先を変えない。
		branchPolicyCase{command: "cd ../main & git switch other", cwd: fixture.detached, want: branchPolicyAttach},
	)
	runBranchPolicyCases(t, cases)
}

// 誤爆がこの判定の実害なので、通す操作を厚く保つ。
func TestClassifyBranchAttachAllowsLegitimateOperations(t *testing.T) {
	fixture := newBranchPolicyFixture(t)
	plain := filepath.Join(fixture.tmp, "plain")
	if err := os.Mkdir(plain, 0o700); err != nil {
		t.Fatal(err)
	}
	var cases []branchPolicyCase
	for _, command := range []string{
		"git checkout --detach HEAD",
		"git checkout --detach other",
		"git switch --detach HEAD",
		"git switch -d other",
		"git checkout -- tracked.txt",
		"git checkout other -- tracked.txt",
		// `--` の無いパスの復元と、commit に解決できる ref は HEAD を attach しない。
		"git checkout other tracked.txt",
		"git checkout HEAD tracked.txt",
		"git checkout HEAD",
		"git checkout main~0",
		"git checkout .",
		"git checkout -p",
		"git checkout --ours tracked.txt",
		"git checkout",
		"git checkout origin/remote-only",
		"git checkout HEAD~1",
		"git switch",
		"git symbolic-ref HEAD",
		"git symbolic-ref --short HEAD",
		"git symbolic-ref -m reason HEAD",
		// ワードが subcommand ではない形は、引用の有無にかかわらず通す。
		`git commit -m "see git switch docs"`,
		`git log --oneline --grep "git checkout"`,
		`echo "git checkout other" >> notes.txt`,
		"git branch switch HEAD",
		"make checkout",
		"git commit -m \"$(cat <<'EOF'\nswitch to the new parser\n\ngit checkout other is no longer needed\nEOF\n)\"",
		"cat > notes.md <<'EOF'\ngit checkout other\ndon't switch here\nEOF",
		"git commit -m \"$(cat <<'EOF'\ndon't git checkout other (yet\nEOF\n)\"",
		`echo "branch: $(git branch --show-current)" && git switch --detach`,
		"cat > notes.md <<-EOF\n\tgit switch other\n\tEOF\ngit status",
		"grep switch <<< 'git switch other'",
		// find -exec の `{}` を群の括弧と取り違えない。
		"git checkout --detach other && find . -name '*.go' -exec wc -l {} \\;",
		"git worktree add --detach " + filepath.Join(fixture.tmp, "new-wt") + " HEAD",
		"git worktree add -b feature " + filepath.Join(fixture.tmp, "new-wt") + " HEAD",
	} {
		cases = append(cases, branchPolicyCase{command: command, cwd: fixture.detached, want: branchPolicyAllow})
	}
	for _, command := range []string{
		"git checkout other",
		"git checkout -b new-branch",
		"git switch other",
		"git switch -c new-branch",
		"git symbolic-ref HEAD refs/heads/other",
		"printf other | xargs git checkout",
		"(cd " + fixture.detached + " && git switch --detach) && git checkout other",
	} {
		cases = append(cases, branchPolicyCase{command: command, cwd: fixture.main, want: branchPolicyAllow})
	}
	cases = append(cases,
		// 既に attach 済みの linked worktree から detached へ戻る操作。
		branchPolicyCase{command: "git switch --detach", cwd: fixture.attached, want: branchPolicyAllow},
		branchPolicyCase{command: "git checkout --detach", cwd: fixture.attached, want: branchPolicyAllow},
		// Git の管理下にないディレクトリは Git 自身が失敗する。
		branchPolicyCase{command: "git checkout main", cwd: plain, want: branchPolicyAllow},
		branchPolicyCase{command: "cd " + fixture.main + " && git checkout other", cwd: fixture.detached, want: branchPolicyAllow},
		branchPolicyCase{command: "git -C " + fixture.main + " switch other", cwd: fixture.detached, want: branchPolicyAllow},
		branchPolicyCase{command: "(cd " + fixture.main + " && git switch other)", cwd: fixture.detached, want: branchPolicyAllow},
		// 対象外の subcommand は実行先が未作成でも通す。
		branchPolicyCase{command: "git -C " + filepath.Join(fixture.tmp, "not-created") + ` status --porcelain "git checkout"`, cwd: fixture.main, want: branchPolicyAllow},
		branchPolicyCase{command: "git worktree add --detach " + filepath.Join(fixture.tmp, "not-created") + " HEAD && git -C " + filepath.Join(fixture.tmp, "not-created") + " status", cwd: fixture.main, want: branchPolicyAllow},
	)
	runBranchPolicyCases(t, cases)
}

func TestClassifyBranchAttachFailsClosedWhenUnresolved(t *testing.T) {
	fixture := newBranchPolicyFixture(t)
	var cases []branchPolicyCase
	for _, command := range []string{
		"sh -c 'git switch other'",
		"bash -lc 'git checkout other'",
		"pushd ../main && git switch other",
		"{ git switch other; }",
		"(git switch other",
		"git switch other)",
		"git checkout 'other",
		"GIT_DIR=../main/.git git switch other",
		"git --git-dir=../main/.git switch other",
		"git --work-tree=../main switch other",
		"cd $HOME && git switch other",
		"cd && git switch other",
		"git -C $REPO switch other",
		"git -C ../* switch other",
		// ref が静的でない checkout は attach かどうかを決められない。
		"git checkout $BRANCH",
		"git checkout \"$BRANCH\"",
		"git checkout `cat ref.txt`",
		"git checkout $(cat ref.txt)",
		"git checkout feature-*",
		// 実行時に引数が足される。
		"printf feature | xargs git checkout",
		"printf feature | parallel git checkout",
		"cat <<EOF\n$(git switch other)\nEOF",
		"cat <<'EOF'\ngit switch other",
		// パイプラインの cd が後続へ引き継がれるかは shell によって違う。
		"cd ../main | git switch other",
		"echo | cd ../main; git switch other",
	} {
		cases = append(cases, branchPolicyCase{command: command, cwd: fixture.detached, want: branchPolicyUnresolved})
	}
	future := filepath.Join(fixture.tmp, "future-clone")
	cases = append(cases, branchPolicyCase{
		command: "git clone https://example.invalid/repo " + future + " && cd " + future + " && git checkout -b feature",
		cwd:     fixture.main,
		want:    branchPolicyUnresolved,
	})
	runBranchPolicyCases(t, cases)
}

func TestBranchAttachHookOutputExplainsTheDecision(t *testing.T) {
	fixture := newBranchPolicyFixture(t)
	decide := func(t *testing.T, fixtureName, command, cwd string) preToolUseHookOutput {
		t.Helper()
		payload := recordedPayloadWithCommand(t, fixtureName, command)
		payload = strings.Replace(payload, `"cwd":"/tmp/wx-session/root"`, `"cwd":"`+cwd+`"`, 1)
		output, ok := branchAttachHookOutput(context.Background(), []byte(payload))
		if !ok {
			t.Fatalf("%q in %s was not denied", command, cwd)
		}
		if output.HookSpecificOutput.PermissionDecision != "deny" || output.HookSpecificOutput.UpdatedInput != nil {
			t.Fatalf("output=%+v", output.HookSpecificOutput)
		}
		return output
	}
	attach := decide(t, "claude-2.1.258-pre-tool-use.json", "git switch other", fixture.detached)
	// Codex は deny に updatedInput があるとエラーにするので、Codex の payload でも省かれることを見る。
	decide(t, "codex-0.151.0-pre-tool-use-exec.json", "git checkout -b feature", fixture.detached)
	for _, want := range []string{"linked worktree", "git branch <branch> HEAD", "git push -u origin <branch>", "HEAD:refs/heads/<branch>", "git switch --detach"} {
		if !strings.Contains(attach.HookSpecificOutput.PermissionDecisionReason, want) {
			t.Fatalf("attach reason %q does not mention %q", attach.HookSpecificOutput.PermissionDecisionReason, want)
		}
	}
	future := filepath.Join(fixture.tmp, "future-clone")
	unresolved := decide(t, "claude-2.1.258-pre-tool-use.json", "git clone https://example.invalid/repo "+future+" && cd "+future+" && git checkout -b feature", fixture.main)
	if !strings.Contains(unresolved.HookSpecificOutput.PermissionDecisionReason, "two Bash calls") {
		t.Fatalf("unresolved reason %q does not explain splitting the command", unresolved.HookSpecificOutput.PermissionDecisionReason)
	}
	// cwd を載せない payload は hook プロセスの作業ディレクトリで判定する。
	t.Chdir(fixture.detached)
	if _, ok := branchAttachHookOutput(context.Background(), []byte(`{"tool_input":{"command":"git switch other"}}`)); !ok {
		t.Fatal("a payload without cwd was not judged in the working directory")
	}
	for _, payload := range []string{
		`{"tool_input":{"command":"git status"}}`,
		`{"tool_input":{"command":["git","switch","other"]}}`,
		`{"tool_input":{"file_path":"switch.go"}}`,
		`{"tool_input":`,
	} {
		if output, ok := branchAttachHookOutput(context.Background(), []byte(payload)); ok {
			t.Fatalf("payload %s was decided: %+v", payload, output)
		}
	}
}

// ブランチ attach の判定も管理下 session に限り、worktree add の書き換えと同じ出口で 1 つだけ出す。
func TestPreToolUseDecisionLimitsBranchAttachToManagedSessions(t *testing.T) {
	fixture := newBranchPolicyFixture(t)
	payload := []byte(`{"cwd":"` + fixture.detached + `","tool_input":{"command":"git switch other"}}`)
	if _, ok := preToolUseDecision(context.Background(), payload, fixture.detached, true); !ok {
		t.Fatal("managed session did not deny the branch attach")
	}
	if output, ok := preToolUseDecision(context.Background(), payload, fixture.detached, false); ok {
		t.Fatalf("a session outside wx management decided: %+v", output)
	}
	rewrite := []byte(`{"cwd":"` + fixture.detached + `","tool_input":{"command":"git worktree add --detach /tmp/x HEAD"}}`)
	output, ok := preToolUseDecision(context.Background(), rewrite, fixture.detached, true)
	if !ok || output.HookSpecificOutput.PermissionDecision != "allow" {
		t.Fatalf("worktree add was not rewritten: %+v", output)
	}
}
