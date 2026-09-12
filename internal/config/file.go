package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
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
	if err != nil || !raw.has("language", raw.Language != "") {
		return LanguageEnglish
	}
	if raw.Language != LanguageEnglish && raw.Language != LanguageJapanese {
		return LanguageEnglish
	}
	return raw.Language
}

// LanguageConfigured は raw 設定に language キーが明示されているかを返す。
// 空文字も「明示された不正値」として true になるため、setup は再質問せず設定エラーを表示できる。
func LanguageConfigured(raw Config) bool { return raw.has("language", raw.Language != "") }

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
	strict := data
	if dropRemovedKeys(&doc) {
		// 削除済みキーが書かれたままの既存configを読めるよう、KnownFields(true)へ渡す前に取り除く。
		// 元のdataは行番号を保つためそのまま使い、書き換えは実際に該当キーがあったときだけ行う。
		cleaned, err := yaml.Marshal(&doc)
		if err != nil {
			return Config{}, err
		}
		strict = cleaned
	}
	dec := yaml.NewDecoder(bytes.NewReader(strict))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("decode %s: %w", p, err)
	}
	c.present = collectKeys(&doc)
	return c, nil
}

// removedKeys は過去に存在した設定キーで、書かれていても黙って無視する。
// 値の解釈は行わないため、`wx config set` などによる次回のSaveで file からも消える。
var removedKeys = []string{"pool.git_concurrency_per_repository"}

// dropRemovedKeys は doc から removedKeys の項目を取り除き、1件でも取り除いたらtrueを返す。
func dropRemovedKeys(doc *yaml.Node) bool {
	if len(doc.Content) == 0 {
		return false
	}
	dropped := false
	for _, key := range removedKeys {
		section, leaf, nested := strings.Cut(key, ".")
		if !nested {
			section, leaf = "", key
		}
		mapping := doc.Content[0]
		if section != "" {
			mapping = mappingValue(mapping, section)
		}
		if removeMappingKey(mapping, leaf) {
			dropped = true
		}
	}
	return dropped
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

// removeMappingKey は mapping node から key と値の組を取り除き、取り除いたらtrueを返す。
func removeMappingKey(node *yaml.Node, key string) bool {
	if node == nil || node.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			node.Content = append(node.Content[:i], node.Content[i+2:]...)
			return true
		}
	}
	return false
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
		if value.Kind != yaml.MappingNode || key == "workspaces" || key == "repositories" || key == "sessions.paths" {
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

// leafInterface は scalar config field の値を yaml.Marshal が扱う具象型で返す。Duration はそのまま渡せる。
func leafInterface(fv reflect.Value) any {
	return fv.Interface()
}

func (c Config) MarshalYAML() (any, error) {
	out := map[string]any{}
	if c.has("version", false) {
		out["version"] = c.Version
	}
	walkConfigLeaves(reflect.ValueOf(c), "", func(key string, fv reflect.Value) {
		if !c.has(key, false) {
			return
		}
		setNestedYAMLValue(out, key, leafInterface(fv))
	})
	walkConfigLists(reflect.ValueOf(c), "", func(key string, fv reflect.Value) {
		if !listPresent(c, key) {
			return
		}
		setNestedYAMLValue(out, key, fv.Interface())
	})
	if c.has("workspaces", c.Workspaces != nil) {
		out["workspaces"] = c.Workspaces
	}
	if c.has("repositories", c.Repositories != nil) {
		out["repositories"] = c.Repositories
	}
	return out, nil
}

func setNestedYAMLValue(root map[string]any, key string, value any) {
	parts := strings.Split(key, ".")
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
