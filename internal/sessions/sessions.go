// Package sessions は再開時に履歴を読み、会話の選択結果を返す。
package sessions

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/HappyOnigiri/WX/internal/sessions/config"
	"github.com/HappyOnigiri/WX/internal/sessions/metacache"
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

// openCache はメタデータ cache を開く。利用できないときは nil を返し、直接走査へ戻す。
// 返す関数は cache を閉じるだけなので、cache の有無に関わらず defer できる。
func openCache() (*metacache.Cache, func()) {
	path, err := metacache.DefaultPath()
	if err != nil {
		return nil, func() {}
	}
	cache, err := metacache.Open(path)
	if err != nil {
		return nil, func() {}
	}
	return cache, func() { _ = cache.Close() }
}

// listItem は再開できる会話と、それが scope 内かの判定を組で保つ。
// picker は scope の内外を切り替えて見せるため、走査結果を scope で捨てずにこのフラグで持ち回る。
type listItem struct {
	session scanner.Session
	inScope bool
}

func list(ctx context.Context, cfg config.Config, opts PickOptions) ([]listItem, error) {
	if opts.Tool != "claude" && opts.Tool != "codex" {
		return nil, fmt.Errorf("unsupported agent: %s", opts.Tool)
	}
	cache, closeCache := openCache()
	defer closeCache()
	items, err := scanner.ScanWith(ctx, cfg, scanner.Options{Cache: cache}, opts.Tool)
	if err != nil {
		return nil, err
	}
	result := make([]listItem, 0, len(items))
	for _, item := range items {
		if item.Tool == opts.Tool && item.Target().Resumable() {
			result = append(result, listItem{session: item, inScope: matchesScope(item, opts.Scope)})
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].session.Mtime == result[j].session.Mtime {
			return result[i].session.StableID < result[j].session.StableID
		}
		return result[i].session.Mtime > result[j].session.Mtime
	})
	return result, nil
}

// Lookup は native ID 指定の再開先を、一覧全体の整列を経ずに探す。
func Lookup(ctx context.Context, cfg config.Config, tool, id string) (ResumeTarget, bool, error) {
	cache, closeCache := openCache()
	defer closeCache()
	item, found, err := scanner.Find(ctx, cfg, scanner.Options{Cache: cache}, tool, id)
	if err != nil || !found {
		return ResumeTarget{}, false, err
	}
	target := item.Target()
	if !target.Resumable() {
		return ResumeTarget{}, false, nil
	}
	return target, true, nil
}

func Continue(ctx context.Context, cfg config.Config, opts ContinueOptions) (ResumeTarget, bool, error) {
	items, err := list(ctx, cfg, opts)
	if err != nil {
		return ResumeTarget{}, false, err
	}
	// Continue は scope 内の最新だけを返す契約なので、picker と違い scope 外の会話は候補にしない。
	for _, item := range items {
		if !item.inScope {
			continue
		}
		if opts.Scope != nil && opts.Scope.Annotations[item.session.StableID].InUse {
			continue
		}
		return item.session.Target(), true, nil
	}
	return ResumeTarget{}, false, nil
}

func Pick(ctx context.Context, cfg config.Config, opts PickOptions) (ResumeTarget, error) {
	items, err := list(ctx, cfg, opts)
	if err != nil {
		return ResumeTarget{}, err
	}
	sessionList := make([]scanner.Session, 0, len(items))
	picker := tui.PickOptions{Label: opts.Tool}
	if opts.Scope != nil {
		picker.Label += " · " + opts.Scope.Label
		picker.Annotations = opts.Scope.Annotations
		picker.Scope = &tui.ScopeFilter{InScope: make(map[string]bool, len(items))}
	}
	for _, item := range items {
		sessionList = append(sessionList, item.session)
		if picker.Scope != nil && item.inScope {
			picker.Scope.InScope[item.session.StableID] = true
		}
	}
	return tui.Pick(ctx, sessionList, picker)
}
