// Package sessions は再開時に履歴を読み、会話の選択結果を返す。
package sessions

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/HappyOnigiri/WX/internal/sessions/config"
	"github.com/HappyOnigiri/WX/internal/sessions/scanner"
	"github.com/HappyOnigiri/WX/internal/sessions/tui"
)

type (
	ResumeTarget = scanner.ResumeTarget
	Annotation   = tui.Annotation
)

type (
	ScopeRoot struct{ Prefix, Label string }
	Scope     struct {
		Roots     []ScopeRoot
		StableIDs []string
	}
	PickerScope struct {
		Scope
		Label       string
		Annotations map[string]Annotation
	}
	PickOptions struct {
		Tool  string
		Scope *PickerScope
	}
	ContinueOptions = PickOptions
)

var ErrCancelled = tui.ErrCancelled

func matchesScope(session scanner.Session, scope *PickerScope) bool {
	if scope == nil {
		return true
	}
	for _, id := range scope.StableIDs {
		if id == session.StableID {
			return true
		}
	}
	for _, root := range scope.Roots {
		if root.Prefix == "" || session.CWD == "" {
			continue
		}
		prefix := filepath.Clean(root.Prefix)
		cwd := filepath.Clean(session.CWD)
		if cwd == prefix || strings.HasPrefix(cwd, strings.TrimRight(prefix, string(filepath.Separator))+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func list(ctx context.Context, cfg config.Config, opts PickOptions) ([]scanner.Session, error) {
	if opts.Tool != "claude" && opts.Tool != "codex" {
		return nil, fmt.Errorf("unsupported agent: %s", opts.Tool)
	}
	items, err := scanner.Scan(ctx, cfg, opts.Tool)
	if err != nil {
		return nil, err
	}
	result := make([]scanner.Session, 0, len(items))
	for _, item := range items {
		if item.Tool == opts.Tool && item.Target().Resumable() && matchesScope(item, opts.Scope) {
			result = append(result, item)
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Mtime == result[j].Mtime {
			return result[i].StableID < result[j].StableID
		}
		return result[i].Mtime > result[j].Mtime
	})
	return result, nil
}

func Lookup(ctx context.Context, cfg config.Config, tool, id string) (ResumeTarget, bool, error) {
	items, err := list(ctx, cfg, PickOptions{Tool: tool})
	if err != nil {
		return ResumeTarget{}, false, err
	}
	for _, item := range items {
		if item.SessionID == id {
			return item.Target(), true, nil
		}
	}
	return ResumeTarget{}, false, nil
}

func Continue(ctx context.Context, cfg config.Config, opts ContinueOptions) (ResumeTarget, bool, error) {
	items, err := list(ctx, cfg, opts)
	if err != nil {
		return ResumeTarget{}, false, err
	}
	for _, item := range items {
		if opts.Scope != nil && opts.Scope.Annotations[item.StableID].InUse {
			continue
		}
		return item.Target(), true, nil
	}
	return ResumeTarget{}, false, nil
}

func Pick(ctx context.Context, cfg config.Config, opts PickOptions) (ResumeTarget, error) {
	items, err := list(ctx, cfg, opts)
	if err != nil {
		return ResumeTarget{}, err
	}
	picker := tui.PickOptions{Label: opts.Tool}
	if opts.Scope != nil {
		picker.Label += " · " + opts.Scope.Label
		picker.Annotations = opts.Scope.Annotations
	}
	return tui.Pick(ctx, items, picker)
}
