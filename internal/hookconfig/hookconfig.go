// Package hookconfig は設定済み agent hook が wx に必要な同期 readiness 契約を満たすか判定する。
package hookconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Available は agent の有効 hook 設定が必須 event すべてに有効な同期 wx readiness hook を持つか返す。
// 欠落、不正、無効化、曖昧、安全でない設定は unavailable とする。
func Available(agent string) bool {
	path, ok := readinessHookPaths(agent)
	if !ok {
		return false
	}
	if agent == "codex" && !codexHooksEnabled() {
		return false
	}
	executable, err := CurrentExecutable()
	if err != nil {
		return false
	}
	required := map[string]string{
		"SessionStart":     "session-start",
		"UserPromptSubmit": "user-prompt-submit",
		"PreToolUse":       "pre-tool-use",
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil || len(data) == 0 || len(data) > 4<<20 {
		return false
	}
	return readinessHookDocumentMatches(data, required, executable)
}

func readinessHookPaths(agent string) (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	switch agent {
	case "codex":
		path, err := regularHookPath(filepath.Join(home, ".codex", "hooks.json"))
		return path, err == nil
	case "claude":
		local := filepath.Join(home, ".claude", "settings.local.json")
		if _, err := regularHookPath(local); err == nil {
			// Claude の local settings は settings.json より優先される。
			// 有効な hook set が曖昧になるため、二つの file を merge しない。
			return local, true
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", false
		}
		path, err := regularHookPath(filepath.Join(home, ".claude", "settings.json"))
		return path, err == nil
	default:
		return "", false
	}
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

type readinessHookGroup struct {
	Matcher  json.RawMessage        `json:"matcher"`
	Disabled json.RawMessage        `json:"disabled"`
	Hooks    []readinessHookCommand `json:"hooks"`
}

type readinessHookCommand struct {
	Type                   string          `json:"type"`
	Command                string          `json:"command"`
	Disabled               json.RawMessage `json:"disabled"`
	Async                  json.RawMessage `json:"async"`
	Once                   json.RawMessage `json:"once"`
	Timeout                json.RawMessage `json:"timeout"`
	StatusMessage          json.RawMessage `json:"statusMessage"`
	AdditionalContextLimit json.RawMessage `json:"additionalContextLimit"`
}

func readinessHookDocumentMatches(data []byte, required map[string]string, executable string) bool {
	var document map[string]json.RawMessage
	if decodeJSON(data, &document) != nil {
		return false
	}
	if disabled, valid := boolOption(document["disableAllHooks"]); !valid || disabled {
		return false
	}
	hooksRaw, ok := document["hooks"]
	if !ok {
		return false
	}
	var hooks map[string]json.RawMessage
	if decodeJSON(hooksRaw, &hooks) != nil {
		return false
	}
	for event, command := range required {
		eventRaw, ok := hooks[event]
		if !ok {
			return false
		}
		var groups []readinessHookGroup
		if decodeStrictJSON(eventRaw, &groups) != nil || !readinessHookGroupsMatch(groups, command, event, executable) {
			return false
		}
	}
	return true
}

func readinessHookGroupsMatch(groups []readinessHookGroup, command, event, executable string) bool {
	for _, group := range groups {
		if !readinessHookGroupValid(group) {
			continue
		}
		for _, hook := range group.Hooks {
			if hook.Type != "command" {
				continue
			}
			disabled, _ := boolOption(hook.Disabled)
			if disabled {
				continue
			}
			async, _ := boolOption(hook.Async)
			once, _ := boolOption(hook.Once)
			if async || once {
				continue
			}
			if isExactWXHookCommandForExecutable(hook.Command, command, executable) {
				return true
			}
		}
	}
	return false
}

func readinessHookGroupValid(group readinessHookGroup) bool {
	disabled, valid := boolOption(group.Disabled)
	if !valid || disabled {
		return false
	}
	if !matcherAppliesToEveryEvent(group.Matcher) || len(group.Hooks) == 0 {
		return false
	}
	for _, hook := range group.Hooks {
		if !readinessHookCommandValid(hook) {
			return false
		}
	}
	return true
}

func readinessHookCommandValid(hook readinessHookCommand) bool {
	if hook.Type != "command" && hook.Type != "prompt" && hook.Type != "agent" {
		return false
	}
	if hook.Type == "command" && hook.Command == "" {
		return false
	}
	for _, raw := range []json.RawMessage{hook.Disabled, hook.Async, hook.Once} {
		if _, valid := boolOption(raw); !valid {
			return false
		}
	}
	if !optionalNumberValid(hook.Timeout) || !optionalStringValid(hook.StatusMessage) || !optionalIntegerValid(hook.AdditionalContextLimit) {
		return false
	}
	return true
}

func matcherAppliesToEveryEvent(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	var matcher string
	if json.Unmarshal(raw, &matcher) != nil {
		return false
	}
	switch matcher {
	case "", "*", ".*", "^.*$", "^.+$":
		return true
	default:
		return false
	}
}

func boolOption(raw json.RawMessage) (bool, bool) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false, len(raw) == 0
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, false
	}
	return value, true
}

func optionalStringValid(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	var value string
	return json.Unmarshal(raw, &value) == nil
}

func optionalNumberValid(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	var value float64
	return json.Unmarshal(raw, &value) == nil
}

func optionalIntegerValid(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	var value int
	return json.Unmarshal(raw, &value) == nil
}

func decodeJSON(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func decodeStrictJSON(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
