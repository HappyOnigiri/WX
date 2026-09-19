package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/i18n"
	"github.com/HappyOnigiri/WX/internal/tui"
)

// linkedWorktreeBase は貸出の基準が cwd と食い違う状態を表す。
// wx は linked worktree を repository の main worktree へ解決するため、cwd の HEAD は貸出に反映されない。
type linkedWorktreeBase struct {
	// Path は cwd を含む linked worktree の root、Head はその HEAD の OID。
	Path string
	Head string
	// MainPath は貸出元になる main worktree の root、MainHead はその HEAD の OID。
	MainPath string
	MainHead string
}

// detectLinkedWorktreeBase は cwd が wx 管理外の linked worktree で、貸出元 main worktree と HEAD が異なるときだけ true を返す。
// Git 以外の cwd、main worktree 自身、wx が作った slot、HEAD が一致する場合、Git を引けない場合は false を返し、案内も確認も出さない。
func (c Client) detectLinkedWorktreeBase(ctx context.Context, cwd string) (linkedWorktreeBase, bool) {
	if cwd == "" {
		return linkedWorktreeBase{}, false
	}
	git := &gitx.Runner{Timeout: c.Config.System.Discovery.Timeout.Duration}
	top, err := git.Run(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return linkedWorktreeBase{}, false
	}
	root, err := domain.Canonicalize(strings.TrimSpace(top.Stdout))
	if err != nil {
		return linkedWorktreeBase{}, false
	}
	list, err := git.Run(ctx, string(root), "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return linkedWorktreeBase{}, false
	}
	first := discovery.FirstWorktreePath(list.Stdout)
	if first == "" {
		return linkedWorktreeBase{}, false
	}
	mainPath, err := domain.Canonicalize(first)
	if err != nil || mainPath == root {
		return linkedWorktreeBase{}, false
	}
	// wx の slot も linked worktree だが、貸出と snapshot で HEAD が動くのが前提なので確認の対象にしない。
	if wtRoot, err := config.ExpandHome(c.Config.WorktreeRoot()); err == nil && domain.IsWithin(wtRoot, string(root)) {
		return linkedWorktreeBase{}, false
	}
	head, mainHead := gitHead(ctx, git, string(root)), gitHead(ctx, git, string(mainPath))
	if head == "" || mainHead == "" || head == mainHead {
		return linkedWorktreeBase{}, false
	}
	return linkedWorktreeBase{Path: string(root), Head: head, MainPath: string(mainPath), MainHead: mainHead}, true
}

// gitHead は worktree の HEAD の OID を返す。未生成の HEAD や解決できない場合は空を返す。
func gitHead(ctx context.Context, git *gitx.Runner, dir string) string {
	res, err := git.Run(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
}

// shortOID は案内に載せる短縮 OID を返す。
func shortOID(oid string) string {
	if len(oid) > 12 {
		return oid[:12]
	}
	return oid
}

// confirmLinkedWorktreeBase は cwd が wx 管理外の linked worktree のとき、main worktree の HEAD で貸し出してよいか確認する。
// 確認を出せない経路（interactive=false、端末なし）は従来どおり notice を出して続行し、続行してよければ true を返す。
// fullscreen の agent が起動すると標準出力の notice は流れるため、端末があるときは確認を出して気付ける機会を作る。
func (c Client) confirmLinkedWorktreeBase(ctx context.Context, cwd string, interactive bool) bool {
	base, mismatched := c.detectLinkedWorktreeBase(ctx, cwd)
	if !mismatched {
		return true
	}
	lang := cliLanguage(c)
	localizer := i18n.New(string(lang))
	fmt.Fprintln(os.Stderr, localizer.Localize("cli.linked.detected", map[string]any{
		"Path": base.Path, "Head": shortOID(base.Head), "MainPath": base.MainPath, "MainHead": shortOID(base.MainHead),
	}))
	if !interactive || !tui.IsTerminal(int(os.Stdin.Fd())) || !tui.IsTerminal(int(os.Stderr.Fd())) {
		fmt.Fprintln(os.Stderr, localizer.Localize("cli.linked.no_terminal", nil))
		return true
	}
	answer, err := tui.Select(ctx, os.Stdin, os.Stderr, tui.Selection{
		Title:       localizer.Localize("cli.linked.title", nil),
		Description: base.Path + " -> " + base.MainPath,
		Initial:     0,
		ClearOnExit: true,
		Language:    string(lang),
		Options: []tui.Option{
			{Value: "yes", Label: localizer.Localize("common.yes", nil), Description: localizer.Localize("cli.linked.yes_description", map[string]any{"Head": shortOID(base.MainHead)})},
			{Value: "no", Label: localizer.Localize("common.no", nil), Description: localizer.Localize("cli.linked.no_description", nil)},
		},
	})
	return err == nil && answer == "yes"
}
