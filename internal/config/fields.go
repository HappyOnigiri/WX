package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"
)

type Field struct{ Key, Value string }

// durationType は Duration の reflect.Type。struct だが、設定上は scalar leaf として扱う。
var durationType = reflect.TypeOf(Duration{})

// walkConfigLeaves は v から到達できる scalar（string、int、bool、Duration）を宣言順に走査する。
// unexported、version、map/slice は共通の scalar key 空間から除外する。
func walkConfigLeaves(v reflect.Value, prefix string, visit func(key string, field reflect.Value)) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if sf.PkgPath != "" {
			continue
		}
		tag, _, _ := strings.Cut(sf.Tag.Get("yaml"), ",")
		if tag == "" || tag == "-" {
			continue
		}
		key := tag
		if prefix != "" {
			key = prefix + "." + tag
		}
		fv := v.Field(i)
		switch {
		case fv.Type() == durationType:
			visit(key, fv)
		case fv.Kind() == reflect.Struct:
			walkConfigLeaves(fv, key, visit)
		case key == "version" || fv.Kind() == reflect.Map || fv.Kind() == reflect.Slice:
			continue
		default:
			visit(key, fv)
		}
	}
}

// walkConfigLists は v から到達できる string slice を宣言順に走査する。
// map は動的キーを持つため、workspaces と repositories を含めて対象外にする。
func walkConfigLists(v reflect.Value, prefix string, visit func(key string, field reflect.Value)) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if sf.PkgPath != "" {
			continue
		}
		tag, _, _ := strings.Cut(sf.Tag.Get("yaml"), ",")
		if tag == "" || tag == "-" {
			continue
		}
		key := tag
		if prefix != "" {
			key = prefix + "." + tag
		}
		fv := v.Field(i)
		switch {
		case fv.Kind() == reflect.Slice && fv.Type().Elem().Kind() == reflect.String:
			visit(key, fv)
		case fv.Kind() == reflect.Struct && fv.Type() != durationType:
			walkConfigLists(fv, key, visit)
		}
	}
}

// configField は walkConfigLeaves と同じ形で key の scalar field を探す。scalar leaf でなければ zero Value を返す。
func configField(v reflect.Value, key string) reflect.Value {
	var target reflect.Value
	walkConfigLeaves(v, "", func(k string, fv reflect.Value) {
		if k == key {
			target = fv
		}
	})
	return target
}

func configListField(v reflect.Value, key string) reflect.Value {
	var target reflect.Value
	walkConfigLists(v, "", func(k string, field reflect.Value) {
		if k == key {
			target = field
		}
	})
	return target
}

// Fields はユーザーが設定できる scalar key と現在の実効値を SetField と同じ順序で列挙する。
func Fields(c Config) []Field {
	var fields []Field
	walkConfigLeaves(reflect.ValueOf(c), "", func(key string, fv reflect.Value) {
		var value string
		switch {
		case fv.Type() == durationType:
			value = fv.Interface().(Duration).String()
		case fv.Kind() == reflect.String:
			value = fv.String()
		case fv.Kind() == reflect.Bool:
			value = strconv.FormatBool(fv.Bool())
		default:
			value = strconv.FormatInt(fv.Int(), 10)
		}
		fields = append(fields, Field{key, value})
	})
	return fields
}

// Lists はユーザーが設定できる list key と現在の実効値を宣言順に列挙する。
func Lists(c Config) []Field {
	var fields []Field
	walkConfigLists(reflect.ValueOf(c), "", func(key string, fv reflect.Value) {
		fields = append(fields, Field{key, fmt.Sprintf("%q", fv.Interface().([]string))})
	})
	return fields
}

// SetField は key の scalar field（duration、整数、真偽、文字列）として value を解析し代入する。
// 範囲と enum の検証は後段の Validate が行う。
func SetField(c *Config, key, value string) error {
	field := configField(reflect.ValueOf(c).Elem(), key)
	if !field.IsValid() {
		return fmt.Errorf("unknown config key %q; run wx config to list available keys", key)
	}
	if err := parseInto(field, value); err != nil {
		return err
	}
	if c.present == nil {
		c.present = map[string]bool{}
	}
	c.present[key] = true
	return nil
}

// parseInto は scalar field（duration、整数、真偽、文字列）として value を解析し代入する。
// ポインタ leaf は個別指定で「明示的な 0 / false」と未指定を区別するために使うので、
// 実体を確保してから同じ分岐へ渡す。
func parseInto(field reflect.Value, value string) error {
	if field.Kind() == reflect.Pointer {
		allocated := reflect.New(field.Type().Elem())
		if err := parseInto(allocated.Elem(), value); err != nil {
			return err
		}
		field.Set(allocated)
		return nil
	}
	switch {
	case field.Type() == durationType:
		d, err := time.ParseDuration(value)
		if err != nil {
			return err
		}
		field.Set(reflect.ValueOf(Duration{d}))
	case field.Kind() == reflect.String:
		field.SetString(value)
	case field.Kind() == reflect.Bool:
		b, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		field.SetBool(b)
	default:
		n, err := strconv.Atoi(value)
		if err != nil {
			return err
		}
		field.SetInt(int64(n))
	}
	return nil
}

// AppendList は discovery.exclude または対応ツールの sessions.paths へ値を追加する。
func AppendList(c *Config, key, value string) error {
	list, err := mutableConfigList(c, key)
	if err != nil {
		return err
	}
	if list.IsNil() {
		defaults := Defaults()
		defaultList := configListField(reflect.ValueOf(defaults), key)
		list.Set(reflect.ValueOf(append([]string(nil), defaultList.Interface().([]string)...)))
	}
	target := normalizeListPath(value)
	values := list.Interface().([]string)
	for _, current := range values {
		if current == value || normalizeListPath(current) == target {
			return fmt.Errorf("%q already exists in %s (as %q)", value, key, current)
		}
	}
	list.Set(reflect.ValueOf(append(values, value)))
	markListPresent(c, key)
	return nil
}

// RemoveList は discovery.exclude または対応ツールの sessions.paths から値を削除する。
func RemoveList(c *Config, key, value string) error {
	list, err := mutableConfigList(c, key)
	if err != nil {
		return err
	}
	if list.IsNil() {
		defaults := Defaults()
		defaultList := configListField(reflect.ValueOf(defaults), key)
		values := defaultList.Interface().([]string)
		if len(values) == 0 {
			return fmt.Errorf("%s has no paths configured", key)
		}
		return fmt.Errorf("%s is unset, so the defaults are in effect (%s); use --add to set explicit paths first", key, strings.Join(values, ", "))
	}
	values := list.Interface().([]string)
	target := normalizeListPath(value)
	index := -1
	for i, current := range values {
		if current == value || normalizeListPath(current) == target {
			index = i
			break
		}
	}
	if index < 0 {
		if len(values) == 0 {
			return fmt.Errorf("%q not found in %s", value, key)
		}
		return fmt.Errorf("%q not found in %s; current values: %s", value, key, strings.Join(values, ", "))
	}
	list.Set(reflect.ValueOf(append(values[:index], values[index+1:]...)))
	markListPresent(c, key)
	return nil
}

// ResetList は指定したリストを未設定へ戻し、既定値を再び有効にする。
// present も落とすため設定ファイルからキーごと消える。
// 空リストを書き残すと、Merge の list 分岐が nil でしか既定値を残さないので既定値が潰れる。
func ResetList(c *Config, key string) error {
	list, err := mutableConfigList(c, key)
	if err != nil {
		return err
	}
	list.Set(reflect.Zero(list.Type()))
	clearListPresent(c, key)
	return nil
}

// ResetField は key の scalar field を未設定へ戻し、既定値を再び有効にする。
// present から落とすので Save でキーが消え、次の Merge(Defaults(), raw) が既定値を返す。
func ResetField(c *Config, key string) error {
	if c == nil {
		return errors.New("config is nil")
	}
	field := configField(reflect.ValueOf(c).Elem(), key)
	if !field.IsValid() {
		return fmt.Errorf("unknown config key %q; run wx config to list available keys", key)
	}
	field.Set(reflect.Zero(field.Type()))
	delete(c.present, key)
	return nil
}

// IsListKey は key が --add・--remove・--reset で操作できる list key かを返す。
// 呼び出し側が list と scalar のどちらの操作へ振り分けるかの判定に使う。
func IsListKey(key string) bool {
	return configListField(reflect.ValueOf(Defaults()), key).IsValid()
}

// mutableConfigList は key の list field を返す。
// walkConfigLists が到達できる list（動的キーを持つ map の下は対象外）だけを許可する。
func mutableConfigList(c *Config, key string) (reflect.Value, error) {
	if c == nil {
		return reflect.Value{}, errors.New("config is nil")
	}
	list := configListField(reflect.ValueOf(c).Elem(), key)
	if !list.IsValid() {
		return reflect.Value{}, fmt.Errorf("unknown list config key %q", key)
	}
	return list, nil
}

func markListPresent(c *Config, key string) {
	if c.present == nil {
		c.present = map[string]bool{}
	}
	if strings.HasPrefix(key, "sessions.paths.") {
		c.present["sessions.paths"] = true
		return
	}
	c.present[key] = true
}

// clearListPresent は list の present を落とす。present が nil の Config は
// 記録なしを意味するため作らない。sessions.paths は claude と codex で present を共有し、
// 個別の list が nil かを listPresent が別に見るので触らない。
func clearListPresent(c *Config, key string) {
	if c.present == nil || strings.HasPrefix(key, "sessions.paths.") {
		return
	}
	delete(c.present, key)
}

func normalizeListPath(path string) string {
	if path == "" {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil {
		path = expandTilde(path, home)
	}
	if absolute, err := filepath.Abs(path); err == nil {
		return absolute
	}
	return filepath.Clean(path)
}
