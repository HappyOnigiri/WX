package hookconfig

import (
	"errors"
	"strings"
	"testing"
)

func TestDocumentRoundTripPreservesOrderAndLiterals(t *testing.T) {
	source := `{
  "model": "opus",
  "count": 1.50,
  "flag": true,
  "nothing": null,
  "text": "a<b&c",
  "list": [
    1,
    {
      "nested": "value"
    }
  ],
  "empty": {},
  "emptyList": []
}
`
	document, err := decodeDocument([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(renderDocument(document, documentIndentOf([]byte(source)))); got != source {
		t.Fatalf("round trip changed the document:\n%s", got)
	}
	if got := topLevelKeyOrder(source); got != "model count flag nothing text list empty emptyList" {
		t.Fatalf("key order=%s", got)
	}
}

// TestDocumentRoundTripPreservesTheOriginalIndent は、4 space やタブの文書で
// 無関係な行まで diff にならないことを確認する。対象は dotfile リポジトリ配下に置かれていることがある。
func TestDocumentRoundTripPreservesTheOriginalIndent(t *testing.T) {
	for name, unit := range map[string]string{"four spaces": "    ", "tab": "\t", "one space": " "} {
		t.Run(name, func(t *testing.T) {
			source := "{\n" + unit + "\"hooks\": {\n" + unit + unit + "\"SessionStart\": [\n" + unit + unit + unit + "1\n" + unit + unit + "]\n" + unit + "}\n}\n"
			document, err := decodeDocument([]byte(source))
			if err != nil {
				t.Fatal(err)
			}
			if got := documentIndentOf([]byte(source)); got != unit {
				t.Fatalf("detected indent=%q want %q", got, unit)
			}
			if got := string(renderDocument(document, documentIndentOf([]byte(source)))); got != source {
				t.Fatalf("round trip changed the indent:\n%s", got)
			}
		})
	}
	// 1 行に収まる文書からは読み取れないので、既定へ落とす。
	if got := documentIndentOf([]byte(`{"a":1}`)); got != "" {
		t.Fatalf("single line indent=%q", got)
	}
	if got := string(renderDocument(&jsonNode{kind: jsonObject}, documentIndentOf([]byte(`{}`)))); got != "{}\n" {
		t.Fatalf("empty document=%q", got)
	}
}

func TestDocumentRejectsDuplicateKeysAndTrailingContent(t *testing.T) {
	if _, err := decodeDocument([]byte(`{"a":1,"a":2}`)); !errors.Is(err, errDuplicateKey) {
		t.Fatalf("duplicate keys err=%v", err)
	}
	if _, err := decodeDocument([]byte(`{"a":{"b":1,"b":2}}`)); !errors.Is(err, errDuplicateKey) {
		t.Fatalf("nested duplicate keys err=%v", err)
	}
	for _, source := range []string{`{"a":1} {"b":2}`, `{"a":1} trailing`, `{`, ``} {
		if _, err := decodeDocument([]byte(source)); err == nil {
			t.Fatalf("malformed document accepted: %q", source)
		}
	}
}

func TestDocumentFieldHelpersEditWithoutReordering(t *testing.T) {
	document, err := decodeDocument([]byte(`{"first":1,"second":2,"third":3}`))
	if err != nil {
		t.Fatal(err)
	}
	document.setField("second", scalarNode("replaced"))
	if !document.removeField("first") || document.removeField("missing") {
		t.Fatal("removeField reported the wrong outcome")
	}
	document.setField("fourth", &jsonNode{kind: jsonArray})
	got := string(renderDocument(document, ""))
	if got != "{\n  \"second\": \"replaced\",\n  \"third\": 3,\n  \"fourth\": []\n}\n" {
		t.Fatalf("edited document:\n%s", got)
	}
	value, ok := document.field("second")
	if !ok {
		t.Fatal("the replaced field is missing")
	}
	if text, ok := value.stringValue(); !ok || text != "replaced" {
		t.Fatalf("stringValue=%q,%v", text, ok)
	}
	if _, ok := value.boolLiteral(); ok {
		t.Fatal("a string was read as a bool literal")
	}
	truth, ok := decodeField(t, `{"flag":true}`, "flag").boolLiteral()
	if !truth || !ok {
		t.Fatal("a true literal was not recognized")
	}
	if _, ok := (*jsonNode)(nil).field("any"); ok {
		t.Fatal("a nil node returned a field")
	}
	if !strings.Contains(string(encodeJSONString("a<b")), "a<b") {
		t.Fatal("HTML escaping changed a plain string")
	}
}

func decodeField(t *testing.T, source, key string) *jsonNode {
	t.Helper()
	document, err := decodeDocument([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	value, ok := document.field(key)
	if !ok {
		t.Fatalf("%s is missing from %s", key, source)
	}
	return value
}
