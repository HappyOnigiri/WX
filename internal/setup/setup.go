// Package setup は wx を単体で使える状態にするための項目を集め、選ばれた操作を適用する。
// TUI・stdin・stdout は持たず、副作用のうち launchctl・socket・RPC は Options の関数として注入される。
// agent hook 設定の wx エントリについては wx が唯一の権威であり、その書き込みそのものは internal/hookconfig が持つ。
// wx setup --json の payload に state.JSONSchemaVersion は載せない。
// あれは wx status --json / wx doctor --json の形状に対する互換契約で、新設コマンドの初回出力はその形状変更ではない。
// commentlint:allow-long -- 権威の所在と JSONSchemaVersion を上げない判断は、この package doc に残すよう計画で決めた
package setup

import (
	"context"
	"fmt"
	"slices"
)

// Action は 1 つの項目に対して選べる操作である。
type Action string

const (
	ActionInstall Action = "install"
	ActionUpdate  Action = "update"
	ActionKeep    Action = "keep"
	ActionRemove  Action = "remove"
	ActionSkip    Action = "skip"
	// ActionDefault と ActionManual は値の決め方を表す。項目の状態から導かれる操作ではなく、値入力を伴う項目の尋ね方としてだけ使う。
	ActionDefault Action = "default"
	ActionManual  Action = "manual"
	// ActionStart は daemon の起動を表す。設定を書かない操作なので install と分ける。
	ActionStart Action = "start"
)

// State は 1 つの項目の現在の状態である。
type State string

const (
	// StateAbsent は wx が管理する設定がまだ無いことを示す。
	StateAbsent State = "absent"
	// StatePresent は期待どおりに設定済みであることを示す。
	StatePresent State = "present"
	// StateDivergent は設定はあるが期待と異なることを示す。update を選べる唯一の状態である。
	StateDivergent State = "divergent"
	// StateUnknown は読めない・解決できないなどで判定できないことを示す。操作は提示せず理由だけを見せる。
	StateUnknown State = "unknown"
	// StateNotApplicable は対象そのものが無いことを示す。
	StateNotApplicable State = "not_applicable"
)

// Step は wx setup が提示する 1 項目である。
// Options が空の項目は質問せず、Detail と Reasons だけを見せて次へ進む。
type Step struct {
	ID      string
	Title   string
	State   State
	Detail  string
	Reasons []string
	Target  string
	Current string
	Desired string
	Options []Action
	Default Action
}

// Options は wx setup が起こす副作用のうち、launchctl・socket・RPC に触れるものを注入する。
type Options struct {
	InstallLaunchAgent   func(context.Context) error
	UninstallLaunchAgent func(context.Context) error
	StartDaemon          func(context.Context) error
	ReloadConfig         func(context.Context) error
	// DaemonStatus は daemon へ Status を 1 回送る。responding は応答があったか、error は応答した daemon が壊れていることを示す。
	DaemonStatus func(context.Context) (bool, error)
}

// optionsFor は状態から選択肢と既定を導く。要件である冪等性がここに集約される。
func optionsFor(state State) ([]Action, Action) {
	switch state {
	case StateAbsent:
		return []Action{ActionInstall, ActionSkip}, ActionInstall
	case StatePresent:
		return []Action{ActionKeep, ActionRemove}, ActionKeep
	case StateDivergent:
		return []Action{ActionUpdate, ActionKeep, ActionRemove}, ActionUpdate
	case StateUnknown, StateNotApplicable:
		return nil, ActionKeep
	default:
		return nil, ActionKeep
	}
}

// stepOptions は項目が実際に扱える操作だけを残す。
// 残った選択肢が keep だけになったら質問する意味がないので、選択肢なしとして扱う。
func stepOptions(state State, supported []Action) ([]Action, Action) {
	options, fallback := optionsFor(state)
	var kept []Action
	for _, option := range options {
		if slices.Contains(supported, option) {
			kept = append(kept, option)
		}
	}
	if len(kept) == 0 || (len(kept) == 1 && kept[0] == ActionKeep) {
		return nil, ActionKeep
	}
	if !slices.Contains(kept, fallback) {
		fallback = kept[0]
	}
	return kept, fallback
}

// Collect は全項目の現在の状態を読み取りだけで集める。
func Collect(ctx context.Context, options Options) ([]Step, error) {
	steps := []Step{collectPrerequisites(ctx), collectWorktreeRoot(), collectShellPath(), collectLaunchAgent()}
	for _, agent := range []string{"claude", "codex"} {
		steps = append(steps, collectHooks(agent))
	}
	return append(steps, collectDaemon(ctx, options)), nil
}

// CollectStep は 1 項目だけを集め直す。適用後の確認に使う。
func CollectStep(ctx context.Context, options Options, id string) (Step, error) {
	steps, err := Collect(ctx, options)
	if err != nil {
		return Step{}, err
	}
	for _, step := range steps {
		if step.ID == id {
			return step, nil
		}
	}
	return Step{}, fmt.Errorf("unknown setup step %q", id)
}

// Pending は対応が要る項目、つまり absent または divergent の項目があるかを返す。
func Pending(steps []Step) bool {
	for _, step := range steps {
		if len(step.Options) > 0 && (step.State == StateAbsent || step.State == StateDivergent) {
			return true
		}
	}
	return false
}

// Divergent は update を選べる項目だけを返す。wx setup --update が提示する対象である。
func Divergent(steps []Step) []Step {
	var out []Step
	for _, step := range steps {
		if step.State == StateDivergent && slices.Contains(step.Options, ActionUpdate) {
			out = append(out, step)
		}
	}
	return out
}

// Apply は選ばれた操作を適用する。value は値の入力を伴う項目でだけ使う。
// keep と skip は何もしないので、呼び出し側は結果だけを見ればよい。
// note は利用者へ見せる 1 行の補足で、書き換えた実体や控えの path のように差分から読み取れない事実を返す。
func Apply(ctx context.Context, options Options, step Step, action Action, value string) (note string, err error) {
	if action == ActionKeep || action == ActionSkip {
		return "", nil
	}
	switch step.ID {
	case stepWorktreeRoot:
		return "", applyWorktreeRoot(ctx, options, step, action, value)
	case stepShellPath:
		return "", applyShellPath(step, action)
	case stepLaunchAgent:
		return "", applyLaunchAgent(ctx, options, action)
	case stepHooksClaude, stepHooksCodex:
		return applyHooks(step, action)
	case stepDaemon:
		return "", applyDaemon(ctx, options, action)
	default:
		return "", fmt.Errorf("setup step %q cannot be applied", step.ID)
	}
}
