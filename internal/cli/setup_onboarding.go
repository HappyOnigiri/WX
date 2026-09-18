package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/onboarding"
	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/tui"
)

const initialSetupUsageTimeout = 3 * time.Second

var (
	setupClipboardCommand = exec.CommandContext
	setupPromptSaver      = saveSetupPrompt
	setupSelect           = tui.Select
	setupIsTerminal       = tui.IsTerminal
)

type setupOnboardingDecision struct {
	Repositories []daemon.SetupCheckRepository
	ForceCold    bool
}

func initialSetupInteractive() bool {
	return setupIsTerminal(int(os.Stdin.Fd())) && setupIsTerminal(int(os.Stderr.Fd()))
}

func (c Client) initialSetupRepositories(workspaceRoot string, repositories []daemon.SetupCheckRepository) []daemon.SetupCheckRepository {
	var out []daemon.SetupCheckRepository
	for _, repository := range repositories {
		record := c.Config.RepositoryFor(workspaceRoot, repository.RelativePath, repository.MainPath).Onboarding
		if record.CheckedAt == "" && record.DeclinedAt == "" {
			out = append(out, repository)
		}
	}
	return out
}

func setupFindingsNeedAttention(findings []diag.Finding) bool {
	for _, finding := range findings {
		if finding.Severity == diag.SeverityProblem || finding.Severity == diag.SeverityUnchecked {
			return true
		}
	}
	return false
}

// resolveInitialSetup は貸出前に初回検査の対象を解決し、対話端末でだけ利用者の方針を選ぶ。
func (c Client) resolveInitialSetup(ctx context.Context, cwd string, enabled bool) (setupOnboardingDecision, bool, error) {
	if !enabled || !initialSetupInteractive() {
		return setupOnboardingDecision{}, false, nil
	}
	callCtx, cancel := context.WithTimeout(ctx, c.discoveryTimeout())
	defer cancel()
	var resolved daemon.SetupOnboarding
	if err := c.RPC.Call(callCtx, "ResolveSetupOnboarding", map[string]string{"cwd": cwd}, &resolved); err != nil {
		if rpc.IsUnknownMethod(err) {
			return setupOnboardingDecision{}, false, nil
		}
		return setupOnboardingDecision{}, false, err
	}
	repositories := c.initialSetupRepositories(resolved.SourceWorkspace, resolved.Repositories)
	if len(repositories) == 0 {
		return setupOnboardingDecision{}, false, nil
	}
	localizer := cliLocalizer(c)
	answer, err := setupSelect(ctx, os.Stdin, os.Stderr, tui.Selection{
		Title:       localizer.Localize("cli.setup_check.title", nil),
		Description: localizer.Localize("cli.setup_check.description", map[string]any{"Workspace": resolved.SourceWorkspace}),
		Initial:     0,
		ClearOnExit: true,
		Language:    c.Config.DisplayLanguage(),
		Options: []tui.Option{
			{Value: "check", Label: localizer.Localize("cli.setup_check.check", nil), Description: localizer.Localize("cli.setup_check.check_description", nil)},
			{Value: "later", Label: localizer.Localize("cli.setup_check.later", nil), Description: localizer.Localize("cli.setup_check.later_description", nil)},
			{Value: "decline", Label: localizer.Localize("cli.setup_check.decline", nil), Description: localizer.Localize("cli.setup_check.decline_description", nil)},
		},
	})
	if err != nil {
		return setupOnboardingDecision{}, true, nil
	}
	switch answer {
	case "check":
		return setupOnboardingDecision{Repositories: repositories, ForceCold: true}, false, nil
	case "later":
		return setupOnboardingDecision{}, false, nil
	case "decline":
		now := time.Now().UTC().Format(time.RFC3339)
		if err := recordInitialSetup(repositories, resolved.SourceWorkspace, "", now); err != nil {
			return setupOnboardingDecision{}, false, err
		}
		return setupOnboardingDecision{}, false, nil
	default:
		return setupOnboardingDecision{}, true, nil
	}
}

// finishInitialSetupCheck は検査結果とプロンプトを agent 起動前に提示する。
// complete が偽なら検査記録と続行確認を行わず、次の対話起動で再検査できるようにする。
func (c Client) finishInitialSetupCheck(ctx context.Context, lease daemon.Lease, repositories []daemon.SetupCheckRepository, findings []diag.Finding, complete bool) bool {
	language := cliLanguage(c)
	reply := diag.Resolve(diag.Reply{Findings: findings}, language)
	diag.RenderLanguage(os.Stderr, reply, false, language)
	now := time.Now().UTC().Format(time.RFC3339)
	prompt, err := onboarding.Render(c.Config.DisplayLanguage(), onboarding.Prompt{
		Workspace: lease.SourceWorkspace, SlotPath: lease.Path,
		RecheckCommand: "wx setup-check " + shellQuoteForPrompt(lease.SourceWorkspace),
		Repositories:   setupPromptRepositories(lease, repositories), Findings: findings,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, cliLocalizer(c).Localize("cli.setup_prompt.failed", map[string]any{"Error": err.Error()}))
	} else if path, saveErr := setupPromptSaver(prompt); saveErr != nil {
		fmt.Fprintln(os.Stderr, cliLocalizer(c).Localize("cli.setup_prompt.failed", map[string]any{"Error": saveErr.Error()}))
	} else {
		fmt.Fprintln(os.Stderr, cliLocalizer(c).Localize("cli.setup_prompt.saved", map[string]any{"Path": path}))
		if copyErr := copySetupPrompt(prompt); copyErr != nil {
			fmt.Fprintln(os.Stderr, cliLocalizer(c).Localize("cli.setup_prompt.copy_failed", map[string]any{"Path": path, "Error": copyErr.Error()}))
		}
	}
	if !complete {
		return false
	}
	if err := recordInitialSetup(repositories, lease.SourceWorkspace, now, ""); err != nil {
		fmt.Fprintln(os.Stderr, cliLocalizer(c).Localize("cli.setup_prompt.record_failed", map[string]any{"Error": err.Error()}))
	}
	attention := setupFindingsNeedAttention(findings)
	initial := 0
	if attention {
		initial = 1
	}
	localizer := cliLocalizer(c)
	answer, err := setupSelect(ctx, os.Stdin, os.Stderr, tui.Selection{
		Title:       localizer.Localize("cli.setup_continue.title", nil),
		Description: localizer.Localize("cli.setup_continue.description", nil),
		Initial:     initial,
		Language:    c.Config.DisplayLanguage(),
		Options: []tui.Option{
			{Value: "continue", Label: localizer.Localize("cli.setup_continue.continue", nil), Description: localizer.Localize("cli.setup_continue.continue_description", nil)},
			{Value: "cancel", Label: localizer.Localize("cli.setup_continue.cancel", nil), Description: localizer.Localize("cli.setup_continue.cancel_description", nil)},
		},
	})
	return err == nil && answer == "continue"
}

func setupPromptRepositories(lease daemon.Lease, repositories []daemon.SetupCheckRepository) []onboarding.Repository {
	out := make([]onboarding.Repository, 0, len(repositories))
	for _, repository := range repositories {
		slotPath := lease.Path
		if len(lease.RepositoryDirs) > 0 {
			slotPath = filepath.Join(lease.Path, repository.DirName)
		}
		out = append(out, onboarding.Repository{RelativePath: repository.RelativePath, MainPath: repository.MainPath, SlotPath: slotPath})
	}
	return out
}

func saveSetupPrompt(prompt string) (string, error) {
	directory, err := os.MkdirTemp("", "wx-setup-")
	if err != nil {
		return "", err
	}
	// wx の終了後に利用者が貼り付けるため、一時 directory は意図的に残す。
	path := filepath.Join(directory, "prompt.md")
	if err := os.WriteFile(path, []byte(prompt), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func copySetupPrompt(prompt string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := setupClipboardCommand(ctx, "pbcopy")
	cmd.Stdin = bytes.NewBufferString(prompt)
	return cmd.Run()
}

func recordInitialSetup(repositories []daemon.SetupCheckRepository, workspaceRoot, checkedAt, declinedAt string) error {
	raw, err := config.LoadRaw()
	if err != nil {
		return err
	}
	for _, repository := range repositories {
		if err := config.SetRepositoryOnboarding(&raw, workspaceRoot, repository.RelativePath, repository.MainPath, checkedAt, declinedAt); err != nil {
			return err
		}
	}
	effective := config.Merge(config.Defaults(), raw)
	if err := config.NormalizePaths(&effective); err != nil {
		return err
	}
	if err := config.Validate(&effective); err != nil {
		return err
	}
	return config.Save(raw)
}

func shellQuoteForPrompt(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
