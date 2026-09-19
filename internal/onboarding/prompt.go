package onboarding

import (
	"bytes"
	"embed"
	"fmt"
	"text/template"

	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/i18n"
)

//go:embed prompt_en.md prompt_ja.md
var promptFiles embed.FS

type Repository struct {
	RelativePath string
	MainPath     string
	SlotPath     string
}

type Prompt struct {
	Workspace      string
	SlotPath       string
	RecheckCommand string
	Repositories   []Repository
	Findings       []diag.Finding
}

// Render は検査結果から通常の agent session へ貼るセットアップ依頼を組み立てる。
func Render(language string, data Prompt) (string, error) {
	name := "prompt_en.md"
	lang := i18n.English
	if i18n.Normalize(language) == i18n.Japanese {
		name, lang = "prompt_ja.md", i18n.Japanese
	}
	data.Findings = diag.Resolve(diag.Reply{Findings: data.Findings}, lang).Findings
	source, err := promptFiles.ReadFile(name)
	if err != nil {
		return "", err
	}
	tmpl, err := template.New(name).Funcs(template.FuncMap{
		"severity": func(value diag.Severity) string { return string(value) },
	}).Parse(string(source))
	if err != nil {
		return "", err
	}
	var output bytes.Buffer
	if err := tmpl.Execute(&output, data); err != nil {
		return "", fmt.Errorf("render setup prompt: %w", err)
	}
	return output.String(), nil
}
