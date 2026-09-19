package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

func Load() (Config, error) {
	effective, _, err := LoadWithRaw()
	return effective, err
}

// LoadLanguage は設定全体を検証せず、表示言語だけを安全に読み取る。
// 起動初期の help・doctor は壊れた別項目があっても英語で利用できる必要があるため、
// 読み取り失敗・未対応値・未記載をすべて英語へ戻す。
func LoadLanguage() string {
	raw, err := LoadRaw()
	if err != nil {
		return LanguageEnglish
	}
	language, explicit := raw.rawLanguage()
	if !explicit || (language != LanguageEnglish && language != LanguageJapanese) {
		return LanguageEnglish
	}
	return language
}

// LanguageConfigured は raw 設定に language キーが明示されているかを返す。
// 空文字も「明示された不正値」として true になるため、setup は再質問せず設定エラーを表示できる。
func LanguageConfigured(raw Config) bool {
	_, explicit := raw.rawLanguage()
	return explicit
}

// LoadWithRaw は検証・正規化済みの実効設定と、値の明示指定を識別できる raw 設定を返す。
func LoadWithRaw() (Config, Config, error) {
	raw, err := LoadRaw()
	if err != nil {
		return Config{}, Config{}, err
	}
	effective := Merge(Defaults(), raw)
	if err := NormalizePaths(&effective); err != nil {
		return Config{}, Config{}, err
	}
	if err := Validate(&effective); err != nil {
		return Config{}, Config{}, err
	}
	return effective, raw, nil
}

func LoadRaw() (Config, error) {
	p, err := Path()
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, err
	}
	scan := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := scan.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("decode %s: %w", p, err)
	}
	var extra any
	if err := scan.Decode(&extra); !errors.Is(err, io.EOF) {
		return Config{}, errors.New("config contains multiple YAML documents")
	}
	if err := requireV2Document(&doc); err != nil {
		return Config{}, fmt.Errorf("decode %s: %w", p, err)
	}
	var c Config
	// 未知のキーは値の解釈から外すだけで読み込みを失敗させない。報告は doctor が行う。
	if err := yaml.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("decode %s: %w", p, err)
	}
	unknown, err := detectUnknownKeys(data, &doc)
	if err != nil {
		return Config{}, fmt.Errorf("decode %s: %w", p, err)
	}
	c.unknown = unknown
	c.present = collectKeys(&doc)
	return c, nil
}

// requireV2Document は既存ファイルの schema 選択を version の値だけで決める。
// version 省略を v1 と推測すると疎な v2 と旧 flat config を区別できないため、
// version: 2 以外は読み込み前に拒否する。
func requireV2Document(doc *yaml.Node) error {
	if doc == nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return errors.New("config version is required; expected version: 2")
	}
	version := mappingValue(doc.Content[0], "version")
	if version == nil {
		return errors.New("config version is required; expected version: 2")
	}
	if version.Kind != yaml.ScalarNode || version.Tag != "!!int" {
		return fmt.Errorf("unsupported config version %q; expected integer 2", version.Value)
	}
	value, err := strconv.Atoi(version.Value)
	if err != nil {
		return fmt.Errorf("unsupported config version %q; expected integer 2", version.Value)
	}
	if value != 2 {
		return fmt.Errorf("unsupported config version %d; expected 2", value)
	}
	return nil
}

// mappingValue は mapping node の key に対応する値を返す。mapping でなければ nil を返す。
func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func collectKeys(doc *yaml.Node) map[string]bool {
	out := map[string]bool{}
	if len(doc.Content) == 0 {
		return out
	}
	collectMappingKeys(doc.Content[0], "", out)
	return out
}

func collectMappingKeys(node *yaml.Node, prefix string, out map[string]bool) {
	if node.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode, value := node.Content[i], node.Content[i+1]
		key := keyNode.Value
		if prefix != "" {
			key = prefix + "." + key
		}
		out[key] = true
		if value.Kind != yaml.MappingNode {
			continue
		}
		if key == "workspaces" || key == "repositories" {
			// map 配下は利用者が選ぶ workspace root と repository membership path なので、
			// 子孫も presence map へ記録し、明示した空 list を往復で保つ。
			for j := 0; j+1 < len(value.Content); j += 2 {
				dynamicKey, dynamicValue := value.Content[j], value.Content[j+1]
				dynamicPath := key + "." + dynamicKey.Value
				out[dynamicPath] = true
				collectMappingKeys(dynamicValue, dynamicPath, out)
			}
			continue
		}
		collectMappingKeys(value, key, out)
	}
}

func (c Config) has(key string, fallback bool) bool {
	if c.present != nil {
		return c.present[key]
	}
	return fallback
}

func Save(c Config) error {
	// 旧 in-memory caller が flatten field を変更していても、保存時には
	// canonical v2 section へ一度だけ反映してから YAML を組み立てる。
	c = withLegacyAdapter(c)
	if c.Version == 0 {
		// 新規の空 Config も保存時に v2 document として確定する。
		c.Version = 2
	}
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if info, statErr := os.Lstat(p); statErr == nil {
		if !info.Mode().IsRegular() {
			return errors.New("config path is not a regular file")
		}
		mode = info.Mode().Perm()
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".config-*.yaml")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, p); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(p))
	if err == nil {
		err = dir.Sync()
		_ = dir.Close()
	}
	return err
}

func (c Config) MarshalYAML() (any, error) {
	known, err := c.marshalKnownYAML()
	if err != nil {
		return nil, err
	}
	return c.applyUnknownKeys(known)
}

// marshalKnownYAML は wx が解釈するキーだけで出力を組み立てる。
func (c Config) marshalKnownYAML() (any, error) {
	return c.marshalV2YAML()
}

// marshalV2YAML は v2 namespace だけを書き出す。legacy の flatten field は
// memory 内の互換 projection であり、v2 file へ漏らしてはならない（次回の strict
// load が二つの正本を検出するため）。
func (c Config) marshalV2YAML() (any, error) {
	if c.Version != 2 {
		return nil, fmt.Errorf("unsupported config version %d; expected 2", c.Version)
	}
	out := map[string]any{"version": 2}
	if c.has("system", !reflect.ValueOf(c.System).IsZero()) {
		section, err := marshalSectionMap(c.System)
		if err != nil {
			return nil, err
		}
		preserveExplicitValues(section, c, "system")
		out["system"] = section
	}
	if c.has("workspace_defaults", !reflect.ValueOf(c.WorkspaceDefaults).IsZero()) {
		section, err := marshalSectionMap(c.WorkspaceDefaults)
		if err != nil {
			return nil, err
		}
		preserveExplicitValues(section, c, "workspace_defaults")
		out["workspace_defaults"] = section
	}
	if c.has("repository_defaults", !reflect.ValueOf(c.RepositoryDefaults).IsZero()) {
		section, err := marshalSectionMap(c.RepositoryDefaults)
		if err != nil {
			return nil, err
		}
		preserveExplicitValues(section, c, "repository_defaults")
		out["repository_defaults"] = section
	}
	workspaces := c.Workspaces
	legacyRepositoryView := len(workspaces) == 0 && len(c.Repositories) > 0
	if len(workspaces) == 0 && len(c.Repositories) > 0 {
		workspaces = legacyRepositoriesAsWorkspaces(c.Repositories)
	}
	if legacyRepositoryView || c.has("workspaces", workspaces != nil) {
		section, err := marshalSectionMap(workspaces)
		if err != nil {
			return nil, err
		}
		preserveWorkspaceExplicitValues(section, c, workspaces)
		out["workspaces"] = section
	}
	return out, nil
}

func marshalSectionMap(value any) (map[string]any, error) {
	data, err := yaml.Marshal(value)
	if err != nil {
		return nil, err
	}
	var section map[string]any
	if err := yaml.Unmarshal(data, &section); err != nil {
		return nil, err
	}
	if section == nil {
		section = map[string]any{}
	}
	return section, nil
}

// preserveExplicitValues は yaml の omitempty で消える明示値を sparse document
// へ戻す。nil list は未指定のままなので、親の組み込み値を継承する契約を保てる。
func preserveExplicitValues(section map[string]any, c Config, prefix string) {
	for key := range c.present {
		if !strings.HasPrefix(key, prefix+".") {
			continue
		}
		relative := strings.TrimPrefix(key, prefix+".")
		if _, exists := nestedYAMLValue(section, relative); exists {
			continue
		}
		original := v2Field(reflect.ValueOf(canonicalSection(c, prefix)), relative)
		if !shouldPreserveExplicitValue(original) {
			continue
		}
		value := original.Interface()
		if original.Kind() == reflect.Slice {
			value = []string{}
		}
		setNestedYAMLValue(section, relative, value)
	}
}

func shouldPreserveExplicitValue(field reflect.Value) bool {
	if !field.IsValid() {
		return false
	}
	if field.Kind() == reflect.Slice {
		return !field.IsNil() && field.Len() == 0
	}
	if field.Kind() == reflect.Pointer || field.Kind() == reflect.Map || field.Kind() == reflect.Struct && field.Type() != durationType {
		return false
	}
	return field.IsZero()
}

func nestedYAMLValue(root map[string]any, key string) (any, bool) {
	parts := strings.Split(key, ".")
	current := root
	for index, part := range parts {
		value, ok := current[part]
		if !ok {
			return nil, false
		}
		if index == len(parts)-1 {
			return value, true
		}
		nested, ok := value.(map[string]any)
		if !ok {
			return nil, false
		}
		current = nested
	}
	return nil, false
}

func preserveWorkspaceExplicitValues(section map[string]any, c Config, workspaces map[string]Workspace) {
	for root, workspace := range workspaces {
		workspaceSection, ok := section[root].(map[string]any)
		if !ok {
			continue
		}
		preserveDynamicValues(c, root, workspaceSection, workspace, []string{
			"worktree", "copy", "link", "agent.add_dir", "discovery.max_depth", "discovery.exclude",
			"repository_defaults.default_branch", "repository_defaults.dir_source", "repository_defaults.prepare.command",
			"repository_defaults.prepare.inputs", "repository_defaults.prepare.timeout", "repository_defaults.prepare.version",
			"repository_defaults.readiness.mode", "repository_defaults.readiness.early_paths", "repository_defaults.readiness.timeout",
			"repository_defaults.storage.copy_mode",
		})
		repositories, ok := workspaceSection["repositories"].(map[string]any)
		if !ok {
			continue
		}
		for relative, repository := range workspace.Repositories {
			repositorySection, ok := repositories[relative].(map[string]any)
			if !ok {
				continue
			}
			preserveDynamicValues(c, root, repositorySection, repository, []string{
				"default_branch", "dir_name", "dir_source", "prepare.command", "prepare.inputs", "prepare.timeout", "prepare.version",
				"readiness.mode", "readiness.early_paths", "readiness.timeout", "storage.copy_mode",
			}, "repositories."+relative)
		}
	}
}

func preserveDynamicValues(c Config, root string, section map[string]any, value any, keys []string, prefix ...string) {
	fieldPrefix := ""
	if len(prefix) > 0 {
		fieldPrefix = prefix[0] + "."
	}
	for _, key := range keys {
		suffix := fieldPrefix + key
		if !dynamicV2FieldPresent(c, root, suffix) {
			continue
		}
		if _, exists := nestedYAMLValue(section, key); exists {
			continue
		}
		field := v2Field(reflect.ValueOf(value), key)
		if !shouldPreserveExplicitValue(field) {
			continue
		}
		fieldValue := field.Interface()
		if field.Kind() == reflect.Slice {
			fieldValue = []string{}
		}
		setNestedYAMLValue(section, key, fieldValue)
	}
}

func dynamicV2FieldPresent(c Config, root, suffix string) bool {
	if c.present == nil {
		return false
	}
	path := "workspaces." + root + "." + suffix
	if c.present[path] {
		return true
	}
	for present := range c.present {
		if !strings.HasPrefix(present, "workspaces.") || !strings.HasSuffix(present, "."+suffix) {
			continue
		}
		candidate := strings.TrimSuffix(strings.TrimPrefix(present, "workspaces."), "."+suffix)
		canonical, err := canonicalPath(candidate)
		if err == nil && canonical == root {
			return true
		}
	}
	return false
}

func canonicalSection(c Config, prefix string) any {
	switch prefix {
	case "system":
		return c.System
	case "workspace_defaults":
		return c.WorkspaceDefaults
	case "repository_defaults":
		return c.RepositoryDefaults
	default:
		return struct{}{}
	}
}

func setNestedYAMLValue(root map[string]any, key string, value any) {
	setNestedYAMLPath(root, strings.Split(key, "."), value)
}

func setNestedYAMLPath(root map[string]any, parts []string, value any) {
	if len(parts) == 0 {
		return
	}
	current := root
	for _, part := range parts[:len(parts)-1] {
		nested, _ := current[part].(map[string]any)
		if nested == nil {
			nested = map[string]any{}
			current[part] = nested
		}
		current = nested
	}
	current[parts[len(parts)-1]] = value
}

func listPresent(c Config, key string) bool {
	if strings.HasPrefix(key, "sessions.paths.") {
		if !c.has("sessions.paths", false) {
			return false
		}
		list := configListField(reflect.ValueOf(c), key)
		return list.IsValid() && !list.IsNil()
	}
	return c.has(key, false)
}
