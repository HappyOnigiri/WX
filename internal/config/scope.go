package config

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// Scope は個別指定を持てる単位である。キー路は global の同名キーをそのまま写しているため、
// CLI・検証・表示・継承元の解決はすべて同じ key 空間で回る。
type Scope int

const (
	ScopeWorkspace Scope = iota
	ScopeRepository
)

func (s Scope) String() string {
	if s == ScopeRepository {
		return "repository"
	}
	return "workspace"
}

// section は Config 内の map field 名と present の記録キーを返す。
func (s Scope) section() string {
	if s == ScopeRepository {
		return "repositories"
	}
	return "workspaces"
}

// newEntry は scope の個別指定を1件分、書き換え可能な reflect.Value で返す。
// map の値は addressable でないため、常にここへ複製してから書き換える。
func (s Scope) newEntry() reflect.Value {
	if s == ScopeRepository {
		return reflect.New(reflect.TypeOf(Repository{})).Elem()
	}
	return reflect.New(reflect.TypeOf(Workspace{})).Elem()
}

// scopeGlobalAliases は global と名前が違う既存キーの継承元である。
// これらは global のキー路をミラーする前から使われている歴史的な別名なので、改名せず表で受ける。
var scopeGlobalAliases = map[string]string{
	"worktree":         "worktree.undefined",
	"reuse_standby":    "worktree.reuse_standby",
	"submodules":       "worktree.submodules",
	"warm_count":       "pool.warm_per_workspace",
	"dir_source":       "storage.repo_dir_source",
	"cow_min_size_kib": "storage.cow_min_size_kib",
}

// globalKeyForScopeKey は scope キーの継承元となる global キーを返す。
// 別名表、同じキー路の global 設定、の順で探し、どちらでもなければ scope 固有のキーとして false を返す。
func globalKeyForScopeKey(key string) (string, bool) {
	if alias, ok := scopeGlobalAliases[key]; ok {
		return alias, true
	}
	defaults := reflect.ValueOf(Defaults())
	if configField(defaults, key).IsValid() || configListField(defaults, key).IsValid() {
		return key, true
	}
	return "", false
}

// walkScopeFields は scope 構造体の leaf（scalar、ポインタ scalar、string slice）を宣言順に走査する。
func walkScopeFields(v reflect.Value, prefix string, visit func(key string, field reflect.Value)) {
	t := v.Type()
	for i := range t.NumField() {
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
		case fv.Type() == durationType || fv.Type() == reflect.PointerTo(durationType):
			visit(key, fv)
		case fv.Kind() == reflect.Struct:
			walkScopeFields(fv, key, visit)
		default:
			visit(key, fv)
		}
	}
}

// ScopeKeys は scope で指定できるキーを宣言順に返す。
func ScopeKeys(s Scope) []string {
	var keys []string
	walkScopeFields(s.newEntry(), "", func(key string, _ reflect.Value) {
		keys = append(keys, key)
	})
	return keys
}

// scopeFieldByKey は entry 内の key に対応する field を返す。無ければ zero Value を返す。
func scopeFieldByKey(entry reflect.Value, key string) reflect.Value {
	var target reflect.Value
	walkScopeFields(entry, "", func(k string, fv reflect.Value) {
		if k == key {
			target = fv
		}
	})
	return target
}

// IsScopeListKey は key が --add・--remove・--reset で操作する list key かを返す。
func IsScopeListKey(s Scope, key string) bool {
	field := scopeFieldByKey(s.newEntry(), key)
	return field.IsValid() && field.Kind() == reflect.Slice
}

func unknownScopeKey(s Scope, key string) error {
	return fmt.Errorf("unknown %s config key %q; available keys: %s", s, key, strings.Join(ScopeKeys(s), ", "))
}

// SetScopeField は scope の scalar key へ value を解析して保存する。
// 範囲と enum の検証は後段の Validate が行う。
func SetScopeField(c *Config, s Scope, target, key, value string) error {
	return mutateScope(c, s, target, func(entry reflect.Value) error {
		field := scopeFieldByKey(entry, key)
		switch {
		case !field.IsValid():
			return unknownScopeKey(s, key)
		case field.Kind() == reflect.Slice:
			return fmt.Errorf("%s is a list key; use --add, --remove, or --reset", key)
		}
		return parseInto(field, value)
	})
}

// ResetScopeField は scope の scalar 個別指定を解除し、継承元の値へ戻す。
func ResetScopeField(c *Config, s Scope, target, key string) error {
	return mutateScope(c, s, target, func(entry reflect.Value) error {
		field := scopeFieldByKey(entry, key)
		switch {
		case !field.IsValid():
			return unknownScopeKey(s, key)
		case field.Kind() == reflect.Slice:
			return fmt.Errorf("%s is a list key; use --reset on the list instead", key)
		}
		field.Set(reflect.Zero(field.Type()))
		return nil
	})
}

// AppendScopeList は scope の list key へ値を追加する。
// 個別指定は global list の置き換えなので、未設定からの追加では global の現在の実効値を種にする。
// 種にした時点の値が焼き付き、以後の global 側の変更はこの scope へ伝わらない。
func AppendScopeList(c *Config, s Scope, target, key, value string) error {
	seed, err := scopeListSeed(c, key)
	if err != nil {
		return err
	}
	return mutateScope(c, s, target, func(entry reflect.Value) error {
		list, err := scopeListField(s, entry, key)
		if err != nil {
			return err
		}
		if list.IsNil() {
			list.Set(reflect.ValueOf(seed))
		}
		normalized := normalizeListPath(value)
		values := list.Interface().([]string)
		for _, current := range values {
			if current == value || normalizeListPath(current) == normalized {
				return fmt.Errorf("%q already exists in %s (as %q)", value, key, current)
			}
		}
		list.Set(reflect.ValueOf(append(values, value)))
		return nil
	})
}

// RemoveScopeList は scope の list key から値を削除する。
// 未設定のままでは global の実効値がそのまま効いているため、先に --add で明示的な list を作らせる。
func RemoveScopeList(c *Config, s Scope, target, key, value string) error {
	return mutateScope(c, s, target, func(entry reflect.Value) error {
		list, err := scopeListField(s, entry, key)
		if err != nil {
			return err
		}
		if list.IsNil() {
			return fmt.Errorf("%s has no %s-specific values, so the global value is in effect; use --add to set explicit values first", key, s)
		}
		values := list.Interface().([]string)
		normalized := normalizeListPath(value)
		index := -1
		for i, current := range values {
			if current == value || normalizeListPath(current) == normalized {
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
		return nil
	})
}

// ResetScopeList は scope の list 個別指定を解除し、global list へ戻す。
func ResetScopeList(c *Config, s Scope, target, key string) error {
	return mutateScope(c, s, target, func(entry reflect.Value) error {
		list, err := scopeListField(s, entry, key)
		if err != nil {
			return err
		}
		list.Set(reflect.Zero(list.Type()))
		return nil
	})
}

func scopeListField(s Scope, entry reflect.Value, key string) (reflect.Value, error) {
	field := scopeFieldByKey(entry, key)
	switch {
	case !field.IsValid():
		return reflect.Value{}, unknownScopeKey(s, key)
	case field.Kind() != reflect.Slice:
		return reflect.Value{}, fmt.Errorf("%s is not a list key", key)
	}
	return field, nil
}

// scopeListSeed は個別指定が無い list の初期値、つまり global の現在の実効値を返す。
// 継承元を持たない scope 固有の list は空から始める。
func scopeListSeed(c *Config, key string) ([]string, error) {
	if c == nil {
		return nil, errors.New("config is nil")
	}
	globalKey, ok := globalKeyForScopeKey(key)
	if !ok {
		return []string{}, nil
	}
	effective := Merge(Defaults(), *c)
	list := configListField(reflect.ValueOf(effective), globalKey)
	if !list.IsValid() {
		return []string{}, nil
	}
	return append([]string{}, list.Interface().([]string)...), nil
}

// mutateScope は個別指定1件の読み出し・書き換え・保存と present の記録をまとめて行う。
// 書き換えた結果が完全な zero になった項目は map から消し、疎な YAML を保つ。
// zero 判定は reflect の再帰的な IsZero なので項目を足しても判定表の追記は要らず、明示的な空 list は非 nil のまま残る。
func mutateScope(c *Config, s Scope, target string, apply func(entry reflect.Value) error) error {
	if c == nil {
		return errors.New("config is nil")
	}
	key, err := scopeOverrideKey(c, s, target)
	if err != nil {
		return err
	}
	overrides := scopeMap(c, s)
	entry := s.newEntry()
	if !overrides.IsNil() {
		if existing := overrides.MapIndex(reflect.ValueOf(key)); existing.IsValid() {
			entry.Set(existing)
		}
	}
	if err := apply(entry); err != nil {
		return err
	}
	switch {
	case entry.IsZero():
		if !overrides.IsNil() {
			overrides.SetMapIndex(reflect.ValueOf(key), reflect.Value{})
		}
	default:
		if overrides.IsNil() {
			overrides.Set(reflect.MakeMap(overrides.Type()))
		}
		overrides.SetMapIndex(reflect.ValueOf(key), entry)
	}
	if c.present == nil {
		c.present = map[string]bool{}
	}
	// 空になった map は present を false にして消す。削除ではなく false なのは、
	// present が nil でない Config では has が記録だけを見るためである。
	c.present[s.section()] = overrides.Len() > 0
	return nil
}

func scopeMap(c *Config, s Scope) reflect.Value {
	v := reflect.ValueOf(c).Elem()
	if s == ScopeRepository {
		return v.FieldByName("Repositories")
	}
	return v.FieldByName("Workspaces")
}

// scopeOverrideKey は target に対応する既存の map key を探し、無ければ canonical path を返す。
// 既存キーの表記（`~/...` など）を保ったまま同じ実体への指定を書き込む。
func scopeOverrideKey(c *Config, s Scope, target string) (string, error) {
	canonical, err := canonicalPath(target)
	if err != nil {
		return "", err
	}
	overrides := scopeMap(c, s)
	if overrides.IsNil() {
		return canonical, nil
	}
	for _, key := range overrides.MapKeys() {
		resolved, err := canonicalPath(key.String())
		if err != nil {
			return "", err
		}
		if resolved == canonical {
			return key.String(), nil
		}
	}
	return canonical, nil
}

// ScopeField は設定の一覧行で、Source は scope 名・explicit・global・default・unset のいずれかである。
type ScopeField struct{ Key, Value, Source string }

// GlobalFields は global の実効値を、設定ファイルの明示値か default かとともに返す。
func GlobalFields(c, raw Config) []ScopeField {
	fields := append(Fields(c), Lists(c)...)
	// language は未記載でも実効値（英語）を持つ global 専用キーであり、
	// Fields の疎な一覧から省いている。dashboard の設定画面では未記載時も
	// 変更対象として見せる必要があるため、ここで default 行を補う。
	hasLanguage := false
	for _, field := range fields {
		if field.Key == "language" {
			hasLanguage = true
			break
		}
	}
	if !hasLanguage {
		fields = append([]Field{{Key: "language", Value: c.DisplayLanguage()}}, fields...)
	}
	out := make([]ScopeField, 0, len(fields))
	for _, field := range fields {
		source := "default"
		if globalFieldPresent(raw, field.Key) {
			source = "explicit"
		}
		out = append(out, ScopeField{Key: field.Key, Value: field.Value, Source: source})
	}
	return out
}

func globalFieldPresent(raw Config, key string) bool {
	if list := configListField(reflect.ValueOf(raw), key); list.IsValid() {
		return listPresent(raw, key)
	}
	field := configField(reflect.ValueOf(raw), key)
	return field.IsValid() && raw.has(key, !field.IsZero())
}

// ScopeFields は scope で指定できる全キーの実効値と出どころを宣言順に返す。
// target は NormalizePaths 済み canonical path であることを呼び出し側の契約とする。
func ScopeFields(c Config, s Scope, target string) []ScopeField {
	return scopeFields(c, Config{}, s, target, false)
}

// ResolvedScopeFields は個別指定のない値を、global で明示された値と default まで遡って区別する。
func ResolvedScopeFields(c, raw Config, s Scope, target string) []ScopeField {
	return scopeFields(c, raw, s, target, true)
}

func scopeFields(c, raw Config, s Scope, target string, resolveDefault bool) []ScopeField {
	entry := s.newEntry()
	overrides := scopeMap(&c, s)
	if !overrides.IsNil() {
		if existing := overrides.MapIndex(reflect.ValueOf(target)); existing.IsValid() {
			entry.Set(existing)
		}
	}
	global := reflect.ValueOf(c)
	var fields []ScopeField
	walkScopeFields(entry, "", func(key string, fv reflect.Value) {
		if !fv.IsZero() {
			fields = append(fields, ScopeField{key, formatScopeValue(fv), s.String()})
			return
		}
		globalKey, ok := globalKeyForScopeKey(key)
		if !ok {
			fields = append(fields, ScopeField{key, "", "unset"})
			return
		}
		source := configField(global, globalKey)
		if !source.IsValid() {
			source = configListField(global, globalKey)
		}
		if !source.IsValid() {
			fields = append(fields, ScopeField{key, "", "unset"})
			return
		}
		inheritedFrom := "global"
		if resolveDefault && !globalFieldPresent(raw, globalKey) {
			inheritedFrom = "default"
		}
		fields = append(fields, ScopeField{key, formatScopeValue(source), inheritedFrom})
	})
	return fields
}

// formatScopeValue は設定値を一覧表示用の文字列にする。list は global 表示と同じ引用形式で出す。
func formatScopeValue(fv reflect.Value) string {
	if fv.Kind() == reflect.Pointer {
		if fv.IsNil() {
			return ""
		}
		fv = fv.Elem()
	}
	switch {
	case fv.Type() == durationType:
		return fv.Interface().(Duration).String()
	case fv.Kind() == reflect.Slice:
		return fmt.Sprintf("%q", fv.Interface().([]string))
	case fv.Kind() == reflect.String:
		return fv.String()
	case fv.Kind() == reflect.Bool:
		return strconv.FormatBool(fv.Bool())
	default:
		return strconv.FormatInt(fv.Int(), 10)
	}
}
