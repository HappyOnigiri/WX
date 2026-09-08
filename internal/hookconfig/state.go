package hookconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// Event は wx が登録する hook event と、その event を処理する wx サブコマンドを表す。
// Required は readiness の同期契約に必要な event で、Available はこの集合だけを見る。
type Event struct {
	Name       string
	Subcommand string
	Required   bool
}

// Events は wx が設定する hook event を、設定ファイルへ書く順で返す。
// SessionEnd は Required ではない。既存利用者の Available を退行させないため必須集合には入れず、
// 欠けている場合は返却がプロセス生存判定へ劣化することを finding として報告する。
func Events() []Event {
	return []Event{
		{Name: "SessionStart", Subcommand: "session-start", Required: true},
		{Name: "UserPromptSubmit", Subcommand: "user-prompt-submit", Required: true},
		{Name: "PreToolUse", Subcommand: "pre-tool-use", Required: true},
		{Name: "SessionEnd", Subcommand: "session-end"},
	}
}

// Status は agent 設定にある wx hook エントリの状態である。
type Status int

const (
	// StatusUnsupported は wx が hook を登録できない agent を示す。
	StatusUnsupported Status = iota
	// StatusAbsent は wx のエントリが 1 件も無いことを示す。
	StatusAbsent
	// StatusCurrent は Events() の全 event が受理される形で登録済みであることを示す。
	StatusCurrent
	// StatusStale は wx のエントリはあるが、欠落・別 binary・不正な形で受理されないことを示す。
	StatusStale
	// StatusBlocked は設定ファイルまたは agent 側の policy を理由に判定も書き込みもできないことを示す。
	StatusBlocked
)

func (s Status) String() string {
	switch s {
	case StatusUnsupported:
		return "unsupported"
	case StatusAbsent:
		return "absent"
	case StatusCurrent:
		return "current"
	case StatusStale:
		return "stale"
	case StatusBlocked:
		return "blocked"
	default:
		return "unknown"
	}
}

// FindingCode は判定が hook を受理しなかった理由で、parser の拒否理由と 1 対 1 に対応する。
type FindingCode string

const (
	FindingUnsupportedAgent    FindingCode = "unsupported_agent"
	FindingAgentNotInstalled   FindingCode = "agent_not_installed"
	FindingTargetUnreadable    FindingCode = "target_unreadable"
	FindingTargetNotRegular    FindingCode = "target_not_regular"
	FindingTargetEmpty         FindingCode = "target_empty"
	FindingTargetTooLarge      FindingCode = "target_too_large"
	FindingTargetUnparsable    FindingCode = "target_unparsable"
	FindingTargetDuplicateKey  FindingCode = "target_duplicate_key"
	FindingTargetSymlink       FindingCode = "target_symlink"
	FindingTargetInRepository  FindingCode = "target_in_repository"
	FindingLocalSettingsShadow FindingCode = "local_settings_shadow"
	FindingAllHooksDisabled    FindingCode = "all_hooks_disabled"
	FindingHooksMissing        FindingCode = "hooks_missing"
	FindingEventMissing        FindingCode = "event_missing"
	FindingEventUnknownField   FindingCode = "event_unknown_field"
	FindingGroupRejected       FindingCode = "group_rejected"
	FindingCommandMissing      FindingCode = "command_missing"
	FindingCommandSkipped      FindingCode = "command_skipped"
	FindingCommandOtherBinary  FindingCode = "command_other_binary"
	FindingExecutableUnknown   FindingCode = "executable_unknown"
	FindingCodexFeatureOff     FindingCode = "codex_feature_disabled"
	FindingCodexConfigUnusable FindingCode = "codex_config_unparsable"
)

// Finding は判定の理由 1 件である。Blocking が真の finding があると Available は必ず false になる。
type Finding struct {
	Code     FindingCode
	Event    string
	Path     string
	Detail   string
	Blocking bool
}

func (f Finding) String() string {
	parts := []string{string(f.Code)}
	if f.Event != "" {
		parts = append(parts, "event="+f.Event)
	}
	if f.Detail != "" {
		parts = append(parts, f.Detail)
	}
	return strings.Join(parts, ": ")
}

// State は 1 つの agent の hook 設定の検査結果である。
// Path は検査対象であると同時に Install / Remove の書き込み対象でもあり、読み側と 1 バイトも違わない規則で決める。
type State struct {
	Agent      string
	Path       string
	Shadowed   string
	Executable string
	Status     Status
	Findings   []Finding
	// Matched は event 名ごとに、受理される wx hook が登録済みかを持つ。
	Matched map[string]bool
}

// Blocking は書き込みや判定を妨げる finding だけを返す。
func (s State) Blocking() []Finding {
	var out []Finding
	for _, finding := range s.Findings {
		if finding.Blocking {
			out = append(out, finding)
		}
	}
	return out
}

// Reasons は利用者に見せる 1 行説明の一覧を返す。
func (s State) Reasons() []string {
	out := make([]string, 0, len(s.Findings))
	for _, finding := range s.Findings {
		out = append(out, finding.String())
	}
	return out
}

// TargetPath は agent の hook 設定の読み書き対象を、ファイルが存在しなくても返す。
// 読み取り先と同じ規則で決めることが、書き込み先と読み取り先の食い違いを防ぐ唯一の手段である。
func TargetPath(agent string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch agent {
	case "codex":
		return filepath.Join(home, ".codex", "hooks.json"), nil
	case "claude":
		local := filepath.Join(home, ".claude", "settings.local.json")
		if _, err := regularHookPath(local); err == nil {
			// Claude の local settings は settings.json より優先されるため、書き込み先も local に揃える。
			return local, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		return filepath.Join(home, ".claude", "settings.json"), nil
	default:
		return "", fmt.Errorf("unsupported agent %q", agent)
	}
}

// shadowedPath は claude の local settings が settings.json を遮蔽している場合に、遮蔽された側を返す。
func shadowedPath(agent, target string) string {
	if agent != "claude" || filepath.Base(target) != "settings.local.json" {
		return ""
	}
	shared := filepath.Join(filepath.Dir(target), "settings.json")
	if _, err := os.Lstat(shared); err != nil {
		return ""
	}
	return shared
}

// maxHookConfigSize は読み側が受理する設定ファイルの上限で、書き込み側も同じ値で先に拒否する。
const maxHookConfigSize = 4 << 20

// Inspect は agent の hook 設定を読み、状態と理由を返す。
// error は agent や HOME の解決自体が失敗したときだけ返し、設定ファイル側の問題は Finding で表す。
func Inspect(agent string) (State, error) {
	state := State{Agent: agent, Matched: map[string]bool{}}
	if agent != "claude" && agent != "codex" {
		state.Status = StatusUnsupported
		state.Findings = append(state.Findings, Finding{Code: FindingUnsupportedAgent, Detail: agent, Blocking: true})
		return state, nil
	}
	path, err := TargetPath(agent)
	if err != nil {
		return State{}, err
	}
	state.Path = path
	if shadowed := shadowedPath(agent, path); shadowed != "" {
		state.Shadowed = shadowed
		state.Findings = append(state.Findings, Finding{Code: FindingLocalSettingsShadow, Path: shadowed, Detail: "settings.local.json takes precedence over " + shadowed})
	}
	executable, findings := runningExecutable()
	if len(findings) > 0 {
		state.Status = StatusBlocked
		state.Findings = append(state.Findings, findings...)
		return state, nil
	}
	state.Executable = executable
	if agent == "codex" {
		state.Findings = append(state.Findings, codexPolicyFindings()...)
	}
	state.Findings = append(state.Findings, targetLayoutFindings(path)...)
	data, readFindings, readable := readHookConfig(path)
	state.Findings = append(state.Findings, readFindings...)
	if readable {
		report := inspectDocument(data, eventCommands(), executable)
		state.Matched = report.matched
		state.Findings = append(state.Findings, report.findings...)
	}
	state.Status = documentStatus(state)
	return state, nil
}

// runningExecutable は判定基準となる実行中 wx を返す。特定できない場合は blocking な finding だけを返す。
func runningExecutable() (string, []Finding) {
	executable, err := CurrentExecutable()
	if err != nil {
		return "", []Finding{{Code: FindingExecutableUnknown, Detail: err.Error(), Blocking: true}}
	}
	return executable, nil
}

// eventCommands は Events() を event 名から wx サブコマンドへの map にする。
func eventCommands() map[string]string {
	out := map[string]string{}
	for _, event := range Events() {
		out[event.Name] = event.Subcommand
	}
	return out
}

// readHookConfig は設定ファイルを読み、読み側が受理しない形（非 regular・空・サイズ超過・不正 JSON）を finding にする。
func readHookConfig(path string) ([]byte, []Finding, bool) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, false
	}
	if err != nil {
		return nil, []Finding{{Code: FindingTargetUnreadable, Path: path, Detail: err.Error(), Blocking: true}}, false
	}
	if !info.Mode().IsRegular() {
		return nil, []Finding{{Code: FindingTargetNotRegular, Path: path, Blocking: true}}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, []Finding{{Code: FindingTargetUnreadable, Path: path, Detail: err.Error(), Blocking: true}}, false
	}
	switch {
	case len(data) == 0:
		return nil, []Finding{{Code: FindingTargetEmpty, Path: path, Blocking: true}}, false
	case len(data) > maxHookConfigSize:
		return nil, []Finding{{Code: FindingTargetTooLarge, Path: path, Blocking: true}}, false
	}
	// 判定は読み側と同じ後勝ちの復号で行う。重複キーを blocking にすると、agent 自身は hook を実行できる設定で
	// wx だけが hook 無しと判断し、前面 readiness 待ちの縮退状態へ戻る。fail closed が要るのは編集経路だけである。
	var probe map[string]json.RawMessage
	if err := decodeJSON(data, &probe); err != nil {
		return nil, []Finding{{Code: FindingTargetUnparsable, Path: path, Detail: err.Error(), Blocking: true}}, false
	}
	var findings []Finding
	if _, err := decodeDocument(data); err != nil && errors.Is(err, errDuplicateKey) {
		findings = append(findings, Finding{Code: FindingTargetDuplicateKey, Path: path, Detail: err.Error() + "; wx will not edit this file until the duplicates are removed"})
	}
	return data, findings, true
}

// targetLayoutFindings は dotfile 管理下の設定ファイルを検出する。
// wx は実体側を書くため、リンク元のリポジトリで commit しないと次の apply で書き戻しが消える。
func targetLayoutFindings(path string) []Finding {
	info, err := os.Lstat(path)
	if err != nil {
		return nil
	}
	var out []Finding
	resolved := path
	if info.Mode()&os.ModeSymlink != 0 {
		resolved, err = filepath.EvalSymlinks(path)
		if err != nil {
			return []Finding{{Code: FindingTargetSymlink, Path: path, Detail: err.Error(), Blocking: true}}
		}
		out = append(out, Finding{Code: FindingTargetSymlink, Path: path, Detail: "resolves to " + resolved})
	}
	if repository := enclosingRepository(resolved); repository != "" {
		out = append(out, Finding{Code: FindingTargetInRepository, Path: resolved, Detail: "inside the repository " + repository})
	}
	return out
}

// enclosingRepository は path を含む Git リポジトリの作業ツリー root を返す。
// dotfile リポジトリ配下かを知るだけなので Git は起動せず、.git の有無だけを親方向に辿る。
func enclosingRepository(path string) string {
	directory := filepath.Dir(path)
	for {
		if _, err := os.Lstat(filepath.Join(directory, ".git")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return ""
		}
		directory = parent
	}
}

// documentStatus は finding と event ごとの一致から状態を決める。
func documentStatus(state State) Status {
	if len(state.Blocking()) > 0 {
		return StatusBlocked
	}
	complete := true
	for _, event := range Events() {
		if !state.Matched[event.Name] {
			complete = false
		}
	}
	if complete {
		return StatusCurrent
	}
	stale := []FindingCode{FindingCommandOtherBinary, FindingCommandSkipped, FindingGroupRejected, FindingEventUnknownField}
	for _, finding := range state.Findings {
		if slices.Contains(stale, finding.Code) {
			return StatusStale
		}
	}
	for _, event := range Events() {
		if state.Matched[event.Name] {
			return StatusStale
		}
	}
	return StatusAbsent
}

// Available は agent の有効 hook 設定が必須 event すべてに有効な同期 wx readiness hook を持つか返す。
// 欠落、不正、無効化、曖昧、安全でない設定は unavailable とする。
func Available(agent string) bool {
	state, err := Inspect(agent)
	if err != nil {
		return false
	}
	if len(state.Blocking()) > 0 {
		return false
	}
	for _, event := range Events() {
		if event.Required && !state.Matched[event.Name] {
			return false
		}
	}
	return true
}

// AgentInstalled は agent 本体が PATH 上にあるかを返す。hook を登録する意味があるかの判定に使う。
func AgentInstalled(agent string) bool {
	_, err := exec.LookPath(agent)
	return err == nil
}

// regularHookPath は path が最終的に regular file を指す場合にその path を返す。
// symlink は拒否しない。読み取り専用の user 設定ファイルであり、
// GNU Stow のような symlink 方式の dotfile 管理で置き換えられても実害がないため。
func regularHookPath(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("hook configuration is not a regular file: %s", path)
	}
	return path, nil
}
