package config

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// v2 scope は意図的に明示する。repository target は常に workspace 相対であり、
// main path をキーにした global map entry にはしない。
const (
	V2ScopeSystem             = "system"
	V2ScopeWorkspaceDefaults  = "workspace-defaults"
	V2ScopeRepositoryDefaults = "repository-defaults"
	V2ScopeWorkspace          = "workspace"
	V2ScopeRepository         = "repository"
)

func v2ScopeEntry(c *Config, scope, root, rel string) (reflect.Value, func() error, error) {
	if c == nil {
		return reflect.Value{}, nil, errors.New("config is nil")
	}
	// v2 scope を編集した時点で空または legacy raw も v2 schema として保存する。
	// top-level section の明示印は下の commit closure が付け、version だけの文書との区別を保つ。
	c.Version = 2
	switch scope {
	case V2ScopeSystem:
		return reflect.ValueOf(&c.System).Elem(), func() error { setV2SectionPresent(c, "system", !reflect.ValueOf(c.System).IsZero()); return nil }, nil
	case V2ScopeWorkspaceDefaults:
		return reflect.ValueOf(&c.WorkspaceDefaults).Elem(), func() error {
			setV2SectionPresent(c, "workspace_defaults", !reflect.ValueOf(c.WorkspaceDefaults).IsZero())
			return nil
		}, nil
	case V2ScopeRepositoryDefaults:
		return reflect.ValueOf(&c.RepositoryDefaults).Elem(), func() error {
			setV2SectionPresent(c, "repository_defaults", !reflect.ValueOf(c.RepositoryDefaults).IsZero())
			return nil
		}, nil
	case V2ScopeWorkspace:
		if root == "" {
			return reflect.Value{}, nil, errors.New("workspace root is required")
		}
		if c.Workspaces == nil {
			c.Workspaces = map[string]Workspace{}
		}
		workspaceKey := v2WorkspaceKey(c, root)
		entry := reflect.New(reflect.TypeOf(Workspace{})).Elem()
		if existing, ok := c.Workspaces[workspaceKey]; ok {
			entry.Set(reflect.ValueOf(existing))
		}
		return entry, func() error {
			if entry.IsZero() {
				delete(c.Workspaces, workspaceKey)
			} else {
				c.Workspaces[workspaceKey] = entry.Interface().(Workspace)
			}
			setV2SectionPresent(c, "workspaces", len(c.Workspaces) > 0)
			return nil
		}, nil
	case V2ScopeRepository:
		if root == "" || rel == "" {
			return reflect.Value{}, nil, errors.New("workspace root and repository relative path are required")
		}
		normalized, err := NormalizeRepositoryRelative(rel)
		if err != nil {
			return reflect.Value{}, nil, err
		}
		rel = normalized
		if c.Workspaces == nil {
			c.Workspaces = map[string]Workspace{}
		}
		workspaceKey := v2WorkspaceKey(c, root)
		w := c.Workspaces[workspaceKey]
		if w.Repositories == nil {
			w.Repositories = map[string]Repository{}
		}
		entry := reflect.New(reflect.TypeOf(Repository{})).Elem()
		if existing, ok := w.Repositories[rel]; ok {
			entry.Set(reflect.ValueOf(existing))
		}
		return entry, func() error {
			if entry.IsZero() {
				delete(w.Repositories, rel)
			} else {
				w.Repositories[rel] = entry.Interface().(Repository)
			}
			if len(w.Repositories) == 0 {
				w.Repositories = nil
			}
			if reflect.ValueOf(w).IsZero() {
				delete(c.Workspaces, workspaceKey)
			} else {
				c.Workspaces[workspaceKey] = w
			}
			setV2SectionPresent(c, "workspaces", len(c.Workspaces) > 0)
			return nil
		}, nil
	default:
		return reflect.Value{}, nil, fmt.Errorf("unknown config scope %q", scope)
	}
}

func v2WorkspaceKey(c *Config, root string) string {
	for key := range c.Workspaces {
		canonical, err := canonicalPath(key)
		if err == nil && canonical == root {
			return key
		}
	}
	return root
}

func setV2SectionPresent(c *Config, key string, present bool) {
	if c.present == nil {
		if !present && c.Version != 2 {
			return
		}
		c.present = map[string]bool{}
	}
	switch {
	case present:
		c.present[key] = true
	case c.Version == 2:
		// 最後の field を reset しても空の v2 section は残す。
		// version だけの document は意図的に無効とし、明示した空 section で
		// schema 選択を記録した疎な v2 document を有効に保つ。
		c.present[key] = true
	default:
		delete(c.present, key)
	}
}

func setV2FieldPresent(c *Config, scope, key string, present bool) {
	if c == nil || scope == V2ScopeRepository || key == "" {
		return
	}
	section := scope
	switch scope {
	case V2ScopeWorkspaceDefaults:
		section = "workspace_defaults"
	case V2ScopeRepositoryDefaults:
		section = "repository_defaults"
	}
	full := section + "." + key
	if c.present == nil {
		if !present {
			return
		}
		c.present = map[string]bool{}
	}
	if present {
		c.present[full] = true
	} else {
		delete(c.present, full)
	}
}

func v2Field(entry reflect.Value, key string) reflect.Value {
	parts := strings.Split(key, ".")
	current := entry
	for _, part := range parts {
		for current.Kind() == reflect.Pointer {
			if current.IsNil() {
				if !current.CanSet() {
					// source resolver に渡した map entry など read-only value は、nil の
					// optional field を調べるだけで変更してはならない。
					return reflect.Value{}
				}
				current.Set(reflect.New(current.Type().Elem()))
			}
			current = current.Elem()
		}
		if current.Kind() != reflect.Struct {
			return reflect.Value{}
		}
		found := reflect.Value{}
		for i := 0; i < current.NumField(); i++ {
			tag, _, _ := strings.Cut(current.Type().Field(i).Tag.Get("yaml"), ",")
			if tag == part {
				found = current.Field(i)
				break
			}
		}
		if !found.IsValid() {
			return reflect.Value{}
		}
		current = found
	}
	return current
}

func SetV2Field(c *Config, scope, root, rel, key, value string) error {
	entry, commit, err := v2ScopeEntry(c, scope, root, rel)
	if err != nil {
		return err
	}
	path := key
	if scope == V2ScopeWorkspace && strings.HasPrefix(key, "repository_defaults.") {
		path = strings.TrimPrefix(key, "repository_defaults.")
		entry = entry.FieldByName("RepositoryDefaults")
	}
	field := v2Field(entry, path)
	if !field.IsValid() || field.Kind() == reflect.Map || field.Kind() == reflect.Slice {
		return fmt.Errorf("unknown %s config key %q", scope, key)
	}
	if err := parseInto(field, value); err != nil {
		return err
	}
	setV2FieldPresent(c, scope, key, true)
	if err := commit(); err != nil {
		return err
	}
	return nil
}

func ResetV2Field(c *Config, scope, root, rel, key string) error {
	entry, commit, err := v2ScopeEntry(c, scope, root, rel)
	if err != nil {
		return err
	}
	path := key
	if scope == V2ScopeWorkspace && strings.HasPrefix(key, "repository_defaults.") {
		path = strings.TrimPrefix(key, "repository_defaults.")
		entry = entry.FieldByName("RepositoryDefaults")
	}
	field := v2Field(entry, path)
	if !field.IsValid() || field.Kind() == reflect.Map || field.Kind() == reflect.Slice {
		return fmt.Errorf("unknown %s config key %q", scope, key)
	}
	field.Set(reflect.Zero(field.Type()))
	setV2FieldPresent(c, scope, key, false)
	return commit()
}

func v2ListField(c *Config, scope, root, rel, key string) (reflect.Value, func() error, error) {
	entry, commit, err := v2ScopeEntry(c, scope, root, rel)
	if err != nil {
		return reflect.Value{}, nil, err
	}
	path := key
	if scope == V2ScopeWorkspace && strings.HasPrefix(key, "repository_defaults.") {
		path = strings.TrimPrefix(key, "repository_defaults.")
		entry = entry.FieldByName("RepositoryDefaults")
	}
	field := v2Field(entry, path)
	if !field.IsValid() || field.Kind() != reflect.Slice || field.Type().Elem().Kind() != reflect.String {
		return reflect.Value{}, nil, fmt.Errorf("%s is not a list key", key)
	}
	return field, commit, nil
}

func AppendV2List(c *Config, scope, root, rel, key, value string) error {
	field, commit, err := v2ListField(c, scope, root, rel, key)
	if err != nil {
		return err
	}
	values := cloneStrings(field.Interface().([]string))
	if field.IsNil() {
		seed, seedErr := v2ListSeed(c, scope, root, rel, key)
		if seedErr != nil {
			return seedErr
		}
		values = append(values, seed...)
	}
	for _, current := range values {
		if current == value || normalizeListPath(current) == normalizeListPath(value) {
			return fmt.Errorf("%q already exists in %s", value, key)
		}
	}
	field.Set(reflect.ValueOf(append(values, value)))
	setV2FieldPresent(c, scope, key, true)
	return commit()
}

func v2ListSeed(c *Config, scope, root, rel, key string) ([]string, error) {
	effective := Merge(Defaults(), *c)
	path := key
	if scope == V2ScopeWorkspace && strings.HasPrefix(path, "repository_defaults.") {
		path = strings.TrimPrefix(path, "repository_defaults.")
	}
	var profile any
	switch scope {
	case V2ScopeSystem:
		profile = effective.System
	case V2ScopeWorkspaceDefaults:
		profile = effective.WorkspaceDefaults
	case V2ScopeRepositoryDefaults:
		profile = effective.RepositoryDefaults
	case V2ScopeWorkspace:
		if strings.HasPrefix(key, "repository_defaults.") {
			profile = effective.RepositoryFor(root, ".", "")
		} else {
			profile = effective.WorkspaceFor(root)
		}
	case V2ScopeRepository:
		profile = effective.RepositoryFor(root, rel, "")
	default:
		return nil, fmt.Errorf("unknown v2 config scope %q", scope)
	}
	field := v2Field(reflect.ValueOf(profile), path)
	if !field.IsValid() || field.Kind() != reflect.Slice || field.Type().Elem().Kind() != reflect.String {
		return nil, fmt.Errorf("%s is not a list key", key)
	}
	return cloneStrings(field.Interface().([]string)), nil
}

func RemoveV2List(c *Config, scope, root, rel, key, value string) error {
	field, commit, err := v2ListField(c, scope, root, rel, key)
	if err != nil {
		return err
	}
	values := field.Interface().([]string)
	index := -1
	for i, current := range values {
		if current == value || normalizeListPath(current) == normalizeListPath(value) {
			index = i
			break
		}
	}
	if index < 0 {
		return fmt.Errorf("%q not found in %s", value, key)
	}
	field.Set(reflect.ValueOf(append(values[:index], values[index+1:]...)))
	setV2FieldPresent(c, scope, key, true)
	return commit()
}

func ResetV2List(c *Config, scope, root, rel, key string) error {
	field, commit, err := v2ListField(c, scope, root, rel, key)
	if err != nil {
		return err
	}
	field.Set(reflect.Zero(field.Type()))
	setV2FieldPresent(c, scope, key, false)
	return commit()
}

// V2Fields は scope の v2 実効値と source label を返す。
// caller は実効 Config（通常は Merge(DefaultsV2(), raw)）と、明示値判定だけに使う
// 疎な raw config を渡す。
func V2Fields(c, raw Config, scope, root, rel string) []ScopeField {
	var entry reflect.Value
	switch scope {
	case V2ScopeSystem:
		entry = reflect.ValueOf(c.System)
	case V2ScopeWorkspaceDefaults:
		entry = reflect.ValueOf(c.WorkspaceDefaults)
	case V2ScopeRepositoryDefaults:
		entry = reflect.ValueOf(c.RepositoryDefaults)
	case V2ScopeWorkspace:
		w := c.WorkspaceFor(root)
		entry = reflect.ValueOf(w)
	case V2ScopeRepository:
		entry = reflect.ValueOf(c.RepositoryFor(root, rel, ""))
	default:
		return nil
	}
	var out []ScopeField
	walkV2Fields(entry, "", func(key string, field reflect.Value) {
		if scope == V2ScopeWorkspace && (key == "repositories" || key == "repository_defaults" || key == "submodules" || strings.HasPrefix(key, "repositories.") || strings.HasPrefix(key, "repository_defaults.")) {
			return
		}
		if scope == V2ScopeRepository && key == "dir_name" { /* included below */
		}
		value := formatScopeValue(field)
		source := "default"
		switch scope {
		case V2ScopeSystem:
			if v2RawFieldPresent(raw, "system", key) {
				source = "global"
			}
		case V2ScopeWorkspaceDefaults:
			if v2RawFieldPresent(raw, "workspace_defaults", key) {
				source = "global"
			}
		case V2ScopeRepositoryDefaults:
			if v2RawFieldPresent(raw, "repository_defaults", key) {
				source = "global"
			}
		case V2ScopeWorkspace:
			workspaceExplicit := false
			if w, ok := c.Workspaces[root]; ok {
				field := v2Field(reflect.ValueOf(w), key)
				if field.IsValid() && !field.IsZero() {
					workspaceExplicit = true
				}
			}
			if workspaceExplicit {
				source = "workspace"
			} else if v2RawFieldPresent(raw, "workspace_defaults", key) {
				source = "global"
			} else if globalKey, ok := globalKeyForScopeKey(key); ok && globalFieldPresent(raw, globalKey) {
				source = "global"
			}
		case V2ScopeRepository:
			resolution := c.ResolveRepository(root, rel, "")
			source = resolution.Sources[key]
			if source == "" {
				source = "default"
			}
		}
		out = append(out, ScopeField{Key: key, Value: value, Source: source})
	})
	if scope == V2ScopeWorkspace {
		// workspace-level repository defaults は CLI/TUI で専用 heading に出し、
		// key space は共有する。
		walkV2Fields(reflect.ValueOf(c.WorkspaceFor(root).RepositoryDefaults), "repository_defaults", func(key string, field reflect.Value) {
			value := formatScopeValue(field)
			source := "default"
			relativeKey := strings.TrimPrefix(key, "repository_defaults.")
			if w, ok := c.Workspaces[root]; ok {
				if f := v2Field(reflect.ValueOf(w.RepositoryDefaults), relativeKey); f.IsValid() && !f.IsZero() {
					source = "workspace"
				}
			}
			if source == "default" && v2RawFieldPresent(raw, "repository_defaults", relativeKey) {
				source = "global"
			}
			out = append(out, ScopeField{Key: key, Value: value, Source: source})
		})
	}
	return out
}

// v2RawFieldPresent は sparse な v2 section の明示 key を調べ、実効 built-in と global 指定を混同しない。
// YAML presence map が無い直接生成 raw では、非 zero field を明示値として扱う。
func v2RawFieldPresent(raw Config, section, key string) bool {
	full := section + "." + key
	if raw.present != nil {
		return raw.present[full]
	}
	var value reflect.Value
	switch section {
	case "system":
		value = reflect.ValueOf(raw.System)
	case "workspace_defaults":
		value = reflect.ValueOf(raw.WorkspaceDefaults)
	case "repository_defaults":
		value = reflect.ValueOf(raw.RepositoryDefaults)
	default:
		return false
	}
	field := v2Field(value, key)
	return field.IsValid() && !field.IsZero()
}

func walkV2Fields(v reflect.Value, prefix string, visit func(string, reflect.Value)) {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < v.NumField(); i++ {
		t := v.Type().Field(i)
		if t.PkgPath != "" {
			continue
		}
		tag, _, _ := strings.Cut(t.Tag.Get("yaml"), ",")
		if tag == "" || tag == "-" || tag == "repositories" {
			continue
		}
		key := tag
		if prefix != "" {
			key = prefix + "." + tag
		}
		field := v.Field(i)
		if field.Type() == durationType || field.Kind() != reflect.Struct {
			visit(key, field)
			continue
		}
		walkV2Fields(field, key, visit)
	}
}
