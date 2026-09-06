package main

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestFormatHumanBytesScalesUnitsAndTrimsFractions(t *testing.T) {
	for _, testCase := range []struct {
		value int64
		want  string
	}{
		{value: 0, want: "0 B"},
		{value: 1023, want: "1023 B"},
		{value: 1024, want: "1 KiB"},
		// 端数は小数第 2 位までにし、意味のない 0 と小数点は落とす。
		{value: 1025, want: "1 KiB"},
		{value: 1536, want: "1.5 KiB"},
		{value: 1792, want: "1.75 KiB"},
		{value: 365 * 1024 * 1024, want: "365 MiB"},
		{value: -1536, want: "-1.5 KiB"},
		// 単位表が尽きたら PiB のまま桁を増やし、未定義の単位へ進めない。
		{value: 1 << 60, want: "1024 PiB"},
	} {
		if got := formatHumanBytes(testCase.value); got != testCase.want {
			t.Fatalf("formatHumanBytes(%d)=%q, want %q", testCase.value, got, testCase.want)
		}
	}
}

func TestHumanDurationSecondsBuildsUnitsAndSurvivesMinInt64(t *testing.T) {
	for _, testCase := range []struct {
		seconds int64
		want    string
	}{
		{seconds: 0, want: "0s"},
		{seconds: 59, want: "59s"},
		{seconds: 60, want: "1m"},
		{seconds: 3600, want: "1h"},
		{seconds: 86400, want: "1 day"},
		{seconds: 172800, want: "2 days"},
		{seconds: 90061, want: "1 day 1h 1m 1s"},
		{seconds: 604800, want: "7 days"},
		{seconds: -61, want: "-1m 1s"},
		// 絶対値を int64 に収められない下限でも、オフセットして全桁を出す。
		{seconds: math.MinInt64, want: "-106751991167300 days 15h 30m 8s"},
	} {
		if got := humanDurationSeconds(testCase.seconds); got != testCase.want {
			t.Fatalf("humanDurationSeconds(%d)=%q, want %q", testCase.seconds, got, testCase.want)
		}
	}
	if got := formatDurationSeconds(604800); got != "604800s (7 days)" {
		t.Fatalf("formatDurationSeconds(604800)=%q", got)
	}
}

func TestStatusHomePathAbbreviatesOnlyPathsUnderHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, testCase := range []struct{ path, want string }{
		{path: "", want: ""},
		{path: home, want: "~"},
		{path: home + "/dev/wx", want: "~/dev/wx"},
		// home の兄弟や外側の path は書き換えない。
		{path: home + "-other", want: home + "-other"},
		{path: "/elsewhere", want: "/elsewhere"},
	} {
		if got := statusHomePath(testCase.path); got != testCase.want {
			t.Fatalf("statusHomePath(%q)=%q, want %q", testCase.path, got, testCase.want)
		}
	}
	if got := statusHomeValue(map[string]any{"root": home + "/dev/wx"}, "root"); got != "~/dev/wx" {
		t.Fatalf("statusHomeValue=%q", got)
	}
	if got := statusHomeValue(map[string]any{}, "root"); got != "—" {
		t.Fatalf("statusHomeValue for a missing key=%q", got)
	}
}

func TestStatusPathWithinAcceptsOnlyPathsUnderRoot(t *testing.T) {
	for _, testCase := range []struct {
		path, root string
		want       bool
	}{
		{path: "/a/b", root: "/a", want: true},
		{path: "/a", root: "/a", want: true},
		{path: "/a", root: "/a/b", want: false},
		// 接頭辞が一致するだけの兄弟 directory を配下と誤認しない。
		{path: "/ab", root: "/a", want: false},
		{path: "", root: "/a", want: false},
		{path: "/a", root: "", want: false},
	} {
		if got := statusPathWithin(testCase.path, testCase.root); got != testCase.want {
			t.Fatalf("statusPathWithin(%q,%q)=%v, want %v", testCase.path, testCase.root, got, testCase.want)
		}
	}
}

func TestStatusIntReadsNumericTypesAndRejectsInexactValues(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		value any
		want  int64
		ok    bool
	}{
		{name: "int", value: 7, want: 7, ok: true},
		{name: "int32", value: int32(-7), want: -7, ok: true},
		{name: "uint8", value: uint8(255), want: 255, ok: true},
		{name: "float64 integral", value: float64(3), want: 3, ok: true},
		{name: "json.Number", value: json.Number("9007199254740993"), want: 9007199254740993, ok: true},
		{name: "float32 integral", value: float32(2), want: 2, ok: true},
		// 丸めると値が変わる入力は表示せず、欠測として扱う。
		{name: "float64 fraction", value: 3.5, ok: false},
		{name: "float64 NaN", value: math.NaN(), ok: false},
		{name: "float64 Inf", value: math.Inf(1), ok: false},
		{name: "uint64 overflow", value: uint64(math.MaxUint64), ok: false},
		{name: "json.Number not integral", value: json.Number("1.5"), ok: false},
		{name: "string", value: "3", ok: false},
		{name: "nil", value: nil, ok: false},
	} {
		got, ok := statusInt(map[string]any{"key": testCase.value}, "key")
		if ok != testCase.ok || (ok && got != testCase.want) {
			t.Fatalf("%s: statusInt=(%d,%v), want (%d,%v)", testCase.name, got, ok, testCase.want, testCase.ok)
		}
	}
	if _, ok := statusInt(map[string]any{}, "key"); ok {
		t.Fatal("statusInt accepted a missing key")
	}
}

func TestStatusValueHelpersLabelMissingAndEmptyValues(t *testing.T) {
	object := map[string]any{"text": "value", "blank": "", "nothing": nil, "list": []any{}, "object": map[string]any{}, "count": 3, "flag": true}
	for _, testCase := range []struct{ key, want string }{
		{key: "text", want: "value"},
		{key: "blank", want: "(empty)"},
		{key: "nothing", want: "(null)"},
		{key: "list", want: "(none)"},
		{key: "object", want: "(none)"},
		{key: "missing", want: "—"},
	} {
		if got := statusValue(object, testCase.key); got != testCase.want {
			t.Fatalf("statusValue(%q)=%q, want %q", testCase.key, got, testCase.want)
		}
	}
	// statusValueRaw は欠測を空文字で返し、sort key として "—" を混ぜない。
	if got := statusValueRaw(object, "missing"); got != "" {
		t.Fatalf("statusValueRaw for a missing key=%q", got)
	}
	if got := statusValueRaw(object, "text"); got != "value" {
		t.Fatalf("statusValueRaw=%q", got)
	}
	if got, ok := statusRawString(object, "nothing"); got != "" || !ok {
		t.Fatalf("statusRawString for a null value=(%q,%v)", got, ok)
	}
	if got, ok := statusRawString(object, "count"); got != "3" || !ok {
		t.Fatalf("statusRawString for a number=(%q,%v)", got, ok)
	}
	if _, ok := statusRawString(object, "missing"); ok {
		t.Fatal("statusRawString accepted a missing key")
	}
	if got, ok := statusBool(object, "flag"); !got || !ok {
		t.Fatalf("statusBool=(%v,%v)", got, ok)
	}
	if _, ok := statusBool(object, "count"); ok {
		t.Fatal("statusBool accepted a non-boolean value")
	}
	if got := statusCountOrDash(object, "count"); got != "3" {
		t.Fatalf("statusCountOrDash=%q", got)
	}
	if got := statusCountOrDash(object, "text"); got != "—" {
		t.Fatalf("statusCountOrDash for a non-numeric value=%q", got)
	}
	if got := statusExactBytes(object, "count"); got != "3 bytes" {
		t.Fatalf("statusExactBytes=%q", got)
	}
	if got := statusExactBytes(object, "text"); got != "—" {
		t.Fatalf("statusExactBytes for a non-numeric value=%q", got)
	}
	if got := statusDash(""); got != "—" {
		t.Fatalf("statusDash(\"\")=%q", got)
	}
	if got := statusDash("x"); got != "x" {
		t.Fatalf("statusDash(%q)=%q", "x", got)
	}
}

func TestStatusLocalDateAndZoneLabelFollowTheDisplayLocation(t *testing.T) {
	previousLocation := statusDisplayLocation
	statusDisplayLocation = time.FixedZone("JST", 9*60*60)
	t.Cleanup(func() { statusDisplayLocation = previousLocation })
	if got := statusLocalDate("2026-09-04T22:16:00Z"); got != "09/05 07:16" {
		t.Fatalf("statusLocalDate=%q", got)
	}
	if got := statusLocalDate("not a timestamp"); got != "—" {
		t.Fatalf("statusLocalDate for an unparsable value=%q", got)
	}
	if got := statusZoneLabel(); got != "JST" {
		t.Fatalf("statusZoneLabel=%q", got)
	}
	// 名前のない zone では見出しの列名が消えないよう LOCAL で代替する。
	statusDisplayLocation = time.FixedZone("", 0)
	if got := statusZoneLabel(); got != "LOCAL" {
		t.Fatalf("statusZoneLabel for an unnamed zone=%q", got)
	}
}

func TestWriteStatusFieldSendsLongAndMultilineValuesToContinuationLines(t *testing.T) {
	var short bytes.Buffer
	writeStatusField(&short, "Label", "value")
	if got := short.String(); got != "Label: value\n" {
		t.Fatalf("short field=%q", got)
	}
	var empty bytes.Buffer
	writeStatusField(&empty, "Label", "")
	if got := empty.String(); got != "Label: (empty)\n" {
		t.Fatalf("empty field=%q", got)
	}
	var multiline bytes.Buffer
	writeStatusField(&multiline, "Label", "first\nsecond")
	if got := multiline.String(); got != "Label:\n    first\n    second\n" {
		t.Fatalf("multiline field=%q", got)
	}
	// 120 rune を超える値は折り返して全文を残す。境界の 120 rune は 1 行に収める。
	var atLimit bytes.Buffer
	writeStatusField(&atLimit, "Label", strings.Repeat("あ", 120))
	if got := atLimit.String(); got != "Label: "+strings.Repeat("あ", 120)+"\n" {
		t.Fatalf("120-rune field=%q", got)
	}
	var overLimit bytes.Buffer
	writeStatusField(&overLimit, "Label", strings.Repeat("あ", 121))
	if got := overLimit.String(); got != "Label:\n    "+strings.Repeat("あ", 121)+"\n" {
		t.Fatalf("121-rune field=%q", got)
	}
}

func TestWriteStatusTablePadsEveryColumnButTheLast(t *testing.T) {
	var output bytes.Buffer
	writeStatusTable(&output, []string{"A", "BB"}, [][]string{{"xxx", "y"}, {"z"}})
	// 幅は列ごとの最長値で決まり、末尾列は余白を付けない。列の足りない行は見出しの値で埋まる。
	want := "A   BB\nxxx y\nz   BB\n"
	if got := output.String(); got != want {
		t.Fatalf("table=%q, want %q", got, want)
	}
	var wide bytes.Buffer
	writeStatusTable(&wide, []string{"PATH"}, [][]string{{"あい"}})
	// 幅は rune 数で数え、全角を含む値でも桁が崩れない。
	if got := wide.String(); got != "PATH\nあい\n" {
		t.Fatalf("wide table=%q", got)
	}
}

func TestStatusObjectListAndSortedByOrderByKeyThenID(t *testing.T) {
	if got := statusObjectList("not a list"); got != nil {
		t.Fatalf("statusObjectList for a non-list=%v", got)
	}
	// object でない要素は落とし、表の行に Go の型表現を混ぜない。
	items := statusObjectList([]any{map[string]any{"id": "b", "root": "/x"}, "junk", map[string]any{"id": "a", "root": "/x"}, map[string]any{"id": "c", "root": "/a"}})
	if len(items) != 3 {
		t.Fatalf("statusObjectList=%v", items)
	}
	sorted := statusObjectsSortedBy(items, "root")
	var ids []string
	for _, item := range sorted {
		ids = append(ids, statusValueRaw(item, "id"))
	}
	if strings.Join(ids, ",") != "c,a,b" {
		t.Fatalf("sorted ids=%v", ids)
	}
	// 入力は書き換えない。呼び出し側は RPC payload をそのまま渡す。
	if statusValueRaw(items[0], "id") != "b" {
		t.Fatalf("statusObjectsSortedBy mutated its input: %v", items)
	}
	single := []map[string]any{{"id": "only"}}
	if got := statusObjectsSortedBy(single, "root"); len(got) != 1 || statusValueRaw(got[0], "id") != "only" {
		t.Fatalf("single-element sort=%v", got)
	}
}

func TestAppendStatusUnknownKeepsOnlyUnknownKeysInSortedOrder(t *testing.T) {
	object := map[string]any{"known": 1, "zeta": "z", "alpha": map[string]any{"inner": "i"}}
	pairs := appendStatusUnknown(nil, "slots", object, map[string]bool{"known": true})
	var got []string
	for _, pair := range pairs {
		got = append(got, pair.key+"="+pair.value)
	}
	want := "slots.alpha.inner=i,slots.zeta=z"
	if strings.Join(got, ",") != want {
		t.Fatalf("unknown pairs=%v, want %q", got, want)
	}
}

func TestFormatRetentionValueAndQuarantineReasonKeepNonNumericInput(t *testing.T) {
	if got := formatRetentionValue(86400); got != "86400s (1 day)" {
		t.Fatalf("formatRetentionValue for a number=%q", got)
	}
	// 数値にできない設定値は捨てず、そのまま見せて設定の誤りに気づけるようにする。
	if got := formatRetentionValue("7d"); got != "7d" {
		t.Fatalf("formatRetentionValue for a string=%q", got)
	}
	if got := formatRetentionValue(nil); got != "(null)" {
		t.Fatalf("formatRetentionValue for nil=%q", got)
	}
	if got := statusQuarantineReason(map[string]any{"failure_code": "OWNERSHIP"}); got != "OWNERSHIP" {
		t.Fatalf("statusQuarantineReason=%q", got)
	}
	// 理由が届かない隔離も 1 グループとして数えるため、空文字ではなく明示的な label を返す。
	if got := statusQuarantineReason(map[string]any{}); got != "(unset)" {
		t.Fatalf("statusQuarantineReason for a missing code=%q", got)
	}
}

func TestWriteStatusLineTerminatesEveryLine(t *testing.T) {
	var output bytes.Buffer
	writeStatusLine(&output, "Workspaces")
	writeStatusLine(&output, "")
	if got := output.String(); got != "Workspaces\n\n" {
		t.Fatalf("lines=%q", got)
	}
}
