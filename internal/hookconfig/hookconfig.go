// Package hookconfig は設定済み agent hook が wx の同期 readiness 契約を満たすか判定し、wx 自身のエントリだけを設定ファイルへ書き戻す。
// wx hook エントリについては wx が唯一の権威である。
// 判定（Available / Inspect）と書き込み（Install / Remove）を同じ package に置き、受理条件と出力形式の食い違いを防ぐ。
package hookconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

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
