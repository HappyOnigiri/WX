package hookconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// jsonKind は順序保持モデルのノード種別である。
type jsonKind uint8

const (
	jsonScalar jsonKind = iota
	jsonObject
	jsonArray
)

// jsonNode は agent 設定を部分編集するための順序保持 JSON モデルである。
// map[string]any では Go がキーを昇順に並べ替えるため、触っていないキーの diff まで巻き込んでしまう。
// scalar は元のバイト列をそのまま保ち、object と array だけを再レンダリングする。
type jsonNode struct {
	kind   jsonKind
	raw    []byte
	keys   []string
	values []*jsonNode
	items  []*jsonNode
}

// documentIndent は書き戻し時のインデント幅で、実機の settings.json / hooks.json の書式に合わせる。
const documentIndent = "  "

// errDuplicateKey は同じ object 内に同名キーがある文書を示す。
// encoding/json は後勝ちで読むため、前者を編集すると読み側は後者を見る。編集は fail closed とする。
var errDuplicateKey = errors.New("JSON object contains duplicate keys")

// decodeDocument は JSON 文書を順序保持モデルへ読み込む。
// 値の直前・直後の offset から元のバイト列を切り出すため、scalar の表記（数値の桁、escape）は失われない。
func decodeDocument(data []byte) (*jsonNode, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	node, err := decodeNode(decoder, data)
	if err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	return node, nil
}

func decodeNode(decoder *json.Decoder, data []byte) (*jsonNode, error) {
	before := decoder.InputOffset()
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delimiter, ok := token.(json.Delim); ok {
		switch delimiter {
		case '{':
			return decodeObject(decoder, data)
		case '[':
			return decodeArray(decoder, data)
		default:
			return nil, fmt.Errorf("unexpected JSON delimiter %q", delimiter)
		}
	}
	return &jsonNode{kind: jsonScalar, raw: rawToken(data, before, decoder.InputOffset())}, nil
}

// rawToken は token の前後 offset から、区切り文字と空白を除いた元のバイト列を取り出す。
func rawToken(data []byte, before, after int64) []byte {
	segment := data[before:after]
	return bytes.Clone(bytes.Trim(segment, " \t\r\n,:"))
}

func decodeObject(decoder *json.Decoder, data []byte) (*jsonNode, error) {
	node := &jsonNode{kind: jsonObject}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("unexpected JSON object key %v", token)
		}
		if seen[key] {
			return nil, fmt.Errorf("%w: %q", errDuplicateKey, key)
		}
		seen[key] = true
		value, err := decodeNode(decoder, data)
		if err != nil {
			return nil, err
		}
		node.keys = append(node.keys, key)
		node.values = append(node.values, value)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	return node, nil
}

func decodeArray(decoder *json.Decoder, data []byte) (*jsonNode, error) {
	node := &jsonNode{kind: jsonArray}
	for decoder.More() {
		item, err := decodeNode(decoder, data)
		if err != nil {
			return nil, err
		}
		node.items = append(node.items, item)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	return node, nil
}

// renderDocument は順序保持モデルを 2 space インデントと末尾改行で書き出す。
func renderDocument(node *jsonNode) []byte {
	var out bytes.Buffer
	renderNode(&out, node, "")
	out.WriteByte('\n')
	return out.Bytes()
}

func renderNode(out *bytes.Buffer, node *jsonNode, indent string) {
	switch node.kind {
	case jsonScalar:
		out.Write(node.raw)
	case jsonObject:
		if len(node.keys) == 0 {
			out.WriteString("{}")
			return
		}
		out.WriteString("{\n")
		inner := indent + documentIndent
		for index, key := range node.keys {
			out.WriteString(inner)
			out.Write(encodeJSONString(key))
			out.WriteString(": ")
			renderNode(out, node.values[index], inner)
			if index < len(node.keys)-1 {
				out.WriteByte(',')
			}
			out.WriteByte('\n')
		}
		out.WriteString(indent + "}")
	case jsonArray:
		if len(node.items) == 0 {
			out.WriteString("[]")
			return
		}
		out.WriteString("[\n")
		inner := indent + documentIndent
		for index, item := range node.items {
			out.WriteString(inner)
			renderNode(out, item, inner)
			if index < len(node.items)-1 {
				out.WriteByte(',')
			}
			out.WriteByte('\n')
		}
		out.WriteString(indent + "]")
	}
}

// encodeJSONString は key と文字列値を JSON literal にする。
// HTML escape を切り、agent 設定によくある `<`・`&` を \u 表記へ書き換えない。
func encodeJSONString(value string) []byte {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	// 文字列の Encode は失敗しない。戻り値を握り潰さないよう、失敗時は最小限の literal へ落とす。
	if err := encoder.Encode(value); err != nil {
		return []byte(`""`)
	}
	return bytes.TrimRight(out.Bytes(), "\n")
}

// field は object ノードの key に対応する値を返す。
func (n *jsonNode) field(key string) (*jsonNode, bool) {
	if n == nil || n.kind != jsonObject {
		return nil, false
	}
	for index, name := range n.keys {
		if name == key {
			return n.values[index], true
		}
	}
	return nil, false
}

// setField は key の値を置き換え、無ければ末尾に追加する。
func (n *jsonNode) setField(key string, value *jsonNode) {
	for index, name := range n.keys {
		if name == key {
			n.values[index] = value
			return
		}
	}
	n.keys = append(n.keys, key)
	n.values = append(n.values, value)
}

// removeField は key と値の組を取り除き、取り除いたら true を返す。
func (n *jsonNode) removeField(key string) bool {
	for index, name := range n.keys {
		if name == key {
			n.keys = append(n.keys[:index], n.keys[index+1:]...)
			n.values = append(n.values[:index], n.values[index+1:]...)
			return true
		}
	}
	return false
}

// stringValue は scalar ノードの文字列値を返す。
func (n *jsonNode) stringValue() (string, bool) {
	if n == nil || n.kind != jsonScalar {
		return "", false
	}
	var value string
	if json.Unmarshal(n.raw, &value) != nil {
		return "", false
	}
	return value, true
}

// scalarNode は文字列値の scalar ノードを作る。
func scalarNode(value string) *jsonNode {
	return &jsonNode{kind: jsonScalar, raw: encodeJSONString(value)}
}

// boolLiteral は scalar ノードが true / false のどちらかを返す。JSON の bool 以外では ok が false になる。
func (n *jsonNode) boolLiteral() (value, ok bool) {
	if n == nil || n.kind != jsonScalar {
		return false, false
	}
	switch strings.TrimSpace(string(n.raw)) {
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		return false, false
	}
}
