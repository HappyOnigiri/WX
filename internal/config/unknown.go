package config

import (
	"bytes"
	"errors"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// UnknownKey は設定ファイルに書かれている、wxが解釈しないキーである。
// バージョン違いのwxを行き来しても設定が壊れないよう、読み込みは失敗させずに doctor が報告する。
type UnknownKey struct {
	// Key はドット区切りのキー、Line は設定ファイル上の行番号である。
	Key  string
	Line int
}

// unknownEntry は UnknownKey に、保存時の差し戻しへ使う値ノードを添えたものである。
// 値が mapping や sequence なら部分木ごと保持し、書かれたとおりに書き戻す。
type unknownEntry struct {
	UnknownKey
	// path は親からの mapping key をノードごとに並べたものである。
	// 表示用の Key はドット連結なので、ドットを含む workspace root を分割してしまう。
	// 差し戻し先は必ずこちらで決める。
	path  []string
	value *yaml.Node
}

// UnknownKeys は設定ファイルにあった未知キーを、ファイルでの出現順で返す。
func (c Config) UnknownKeys() []UnknownKey {
	if len(c.unknown) == 0 {
		return nil
	}
	out := make([]UnknownKey, 0, len(c.unknown))
	for _, entry := range c.unknown {
		out = append(out, entry.UnknownKey)
	}
	return out
}

// unknownFieldMessage は yaml.v3 が KnownFields(true) で返す未知フィールドのメッセージである。
// 文言は yaml.v3 の内部実装であり、go.modで固定した版に依存する。解析はこの1箇所に閉じる。
var unknownFieldMessage = regexp.MustCompile(`^line (\d+): field (.+) not found in type \S+$`)

// parseUnknownField はメッセージから行番号とフィールド名を取り出す。
// 未知フィールド以外の型エラーは対象外として false を返す。
func parseUnknownField(message string) (int, string, bool) {
	match := unknownFieldMessage.FindStringSubmatch(message)
	if match == nil {
		return 0, "", false
	}
	line, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, "", false
	}
	return line, match[2], true
}

// detectUnknownKeys は data を strict decode し、未知キーを doc のノードへ引き当てて返す。
// 検出は yaml.v3 自身に任せ、既知キーの一覧を別に持たない。値の解釈は呼び出し側が別に行う。
func detectUnknownKeys(data []byte, doc *yaml.Node) ([]unknownEntry, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var probe Config
	err := dec.Decode(&probe)
	if err == nil {
		return nil, nil
	}
	var typeError *yaml.TypeError
	if !errors.As(err, &typeError) {
		// 未知キー以外の失敗は値の解釈でも出るため、そちらの decode が同じ内容を報告する。
		return nil, err
	}
	claimed := map[*yaml.Node]bool{}
	var out []unknownEntry
	for _, message := range typeError.Errors {
		line, field, ok := parseUnknownField(message)
		if !ok {
			continue
		}
		entry, ok := findUnknownNode(doc, nil, line, field, claimed)
		if !ok {
			continue
		}
		out = append(out, entry)
	}
	return out, nil
}

// findUnknownNode は node から line 行にある名前 field のキーを探し、そこまでのキー列と値ノードを返す。
// 同じ行に同名のキーが並ぶ inline mapping に備え、割り当て済みのキーノードは claimed で除く。
func findUnknownNode(node *yaml.Node, prefix []string, line int, field string, claimed map[*yaml.Node]bool) (unknownEntry, bool) {
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			if entry, ok := findUnknownNode(child, prefix, line, field, claimed); ok {
				return entry, true
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			keyNode, value := node.Content[i], node.Content[i+1]
			// 兄弟のキー列を壊さないよう、prefix の backing array は共有せず毎回作る。
			path := make([]string, 0, len(prefix)+1)
			path = append(append(path, prefix...), keyNode.Value)
			if keyNode.Line == line && keyNode.Value == field && !claimed[keyNode] {
				claimed[keyNode] = true
				return unknownEntry{UnknownKey{Key: strings.Join(path, "."), Line: line}, path, value}, true
			}
			if entry, ok := findUnknownNode(value, path, line, field, claimed); ok {
				return entry, true
			}
		}
	case yaml.ScalarNode, yaml.AliasNode:
		// scalar と alias はキーを持たないため、探索の末端である。
	}
	return unknownEntry{}, false
}

// applyUnknownKeys は既知キーから組み立てた出力へ、未知キーを元の位置と値のまま差し戻す。
// 旧版のwxで保存しただけで新版のキーが消えるのを防ぎ、typoも黙って消さずdoctorに出し続ける。
func (c Config) applyUnknownKeys(known any) (any, error) {
	if len(c.unknown) == 0 {
		return known, nil
	}
	var root yaml.Node
	if err := root.Encode(known); err != nil {
		return nil, err
	}
	for _, entry := range c.unknown {
		insertYAMLNode(&root, entry.path, resolveUnknownAliases(entry.value, map[*yaml.Node]bool{}))
	}
	return &root, nil
}

// resolveUnknownAliases は未知ノードの alias を参照先の内容へ展開し、anchor 名を落とす。
// 既知キーは Go の値から組み直すため anchor 定義が出力に残らず、alias をそのまま
// 書き戻すと保存した設定を次回読み込めない。expanding は展開中の anchor である。
func resolveUnknownAliases(node *yaml.Node, expanding map[*yaml.Node]bool) *yaml.Node {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.AliasNode {
		// 自分自身を含む anchor は展開できないため、書かれたまま残して値を失わない。
		// この形は既知キーの decode が先に拒否するので、保存まで到達しない。
		if node.Alias == nil || expanding[node.Alias] {
			return node
		}
		expanding[node.Alias] = true
		defer delete(expanding, node.Alias)
		return resolveUnknownAliases(node.Alias, expanding)
	}
	clone := *node
	clone.Anchor = ""
	if len(node.Content) > 0 {
		clone.Content = make([]*yaml.Node, len(node.Content))
		for i, child := range node.Content {
			clone.Content[i] = resolveUnknownAliases(child, expanding)
		}
	}
	return &clone
}

// insertYAMLNode は mapping の path 位置へ value を置き、途中の mapping が無ければ作る。
// 途中や置き先が mapping でない場合は表現できないため、その1件の差し戻しを諦める。
func insertYAMLNode(node *yaml.Node, path []string, value *yaml.Node) {
	if len(path) == 0 {
		return
	}
	current := node
	for _, part := range path[:len(path)-1] {
		if current.Kind != yaml.MappingNode {
			return
		}
		next := mappingValue(current, part)
		if next == nil {
			next = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			current.Content = append(current.Content, scalarKeyNode(part), next)
		}
		current = next
	}
	leaf := path[len(path)-1]
	if current.Kind != yaml.MappingNode || mappingValue(current, leaf) != nil {
		return
	}
	current.Content = append(current.Content, scalarKeyNode(leaf), value)
}

func scalarKeyNode(name string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}
}
