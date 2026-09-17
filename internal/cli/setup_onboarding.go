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
)

const initialSetupUsageTimeout = 3 * time.Second

var setupClipboardCommand = exec.CommandContext

func mergeSetupCheckRepositories(values ...[]daemon.SetupCheckRepository) []daemon.SetupCheckRepository {
	seen := map[string]bool{}
	var out []daemon.SetupCheckRepository
	for _, repositories := range values {
		for _, repository := range repositories {
			key := repository.RelativePath + "\x00" + repository.MainPath
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, repository)
		}
	}
	return out
}

func (c Client) initialSetupRepositories(workspaceRoot string, repositories []daemon.SetupCheckRepository) []daemon.SetupCheckRepository {
	var out []daemon.SetupCheckRepository
	for _, repository := range repositories {
		record := c.Config.RepositoryFor(workspaceRoot, repository.RelativePath, repository.MainPath).Onboarding
		if record.CheckedAt == "" && record.PromptedAt == "" {
			out = append(out, repository)
		}
	}
	return out
}

func setupFindingsNeedPrompt(findings []diag.Finding) bool {
	for _, finding := range findings {
		if finding.Severity == diag.SeverityProblem || finding.Severity == diag.SeverityUnchecked {
			return true
		}
	}
	return false
}

func (c Client) finishInitialSetupCheck(lease daemon.Lease, repositories []daemon.SetupCheckRepository, findings []diag.Finding) {
	now := time.Now().UTC().Format(time.RFC3339)
	promptedAt := ""
	if setupFindingsNeedPrompt(findings) {
		prompt, err := onboarding.Render(c.Config.DisplayLanguage(), onboarding.Prompt{
			Workspace: lease.SourceWorkspace, SlotPath: lease.Path,
			RecheckCommand: "wx setup-check " + shellQuoteForPrompt(lease.SourceWorkspace),
			Repositories:   setupPromptRepositories(lease, repositories), Findings: findings,
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, cliLocalizer(c).Localize("cli.setup_prompt.failed", map[string]any{"Error": err.Error()}))
		} else if path, saveErr := saveSetupPrompt(prompt); saveErr != nil {
			fmt.Fprintln(os.Stderr, cliLocalizer(c).Localize("cli.setup_prompt.failed", map[string]any{"Error": saveErr.Error()}))
		} else {
			promptedAt = now
			fmt.Fprintln(os.Stderr, cliLocalizer(c).Localize("cli.setup_prompt.saved", map[string]any{"Path": path}))
			if copyErr := copySetupPrompt(prompt); copyErr != nil {
				fmt.Fprintln(os.Stderr, cliLocalizer(c).Localize("cli.setup_prompt.copy_failed", map[string]any{"Path": path, "Error": copyErr.Error()}))
			}
		}
	}
	if err := recordInitialSetup(repositories, lease.SourceWorkspace, now, promptedAt); err != nil {
		fmt.Fprintln(os.Stderr, cliLocalizer(c).Localize("cli.setup_prompt.record_failed", map[string]any{"Error": err.Error()}))
	}
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

func recordInitialSetup(repositories []daemon.SetupCheckRepository, workspaceRoot, checkedAt, promptedAt string) error {
	raw, err := config.LoadRaw()
	if err != nil {
		return err
	}
	for _, repository := range repositories {
		if err := config.SetRepositoryOnboarding(&raw, workspaceRoot, repository.RelativePath, repository.MainPath, checkedAt, promptedAt); err != nil {
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
