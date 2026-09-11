package textfmt

import (
	"path/filepath"
	"testing"
)

func TestHumanBytesScalesUnitsAndTrimsFractions(t *testing.T) {
	for _, testCase := range []struct {
		value int64
		want  string
	}{
		{value: 0, want: "0 B"},
		{value: 512, want: "512 B"},
		{value: 1023, want: "1023 B"},
		{value: 1024, want: "1 KiB"},
		// 端数は小数第 2 位までにし、意味のない 0 と小数点は落とす。
		{value: 1025, want: "1 KiB"},
		{value: 1536, want: "1.5 KiB"},
		{value: 1792, want: "1.75 KiB"},
		{value: 254464, want: "248.5 KiB"},
		{value: 1024 * 1024, want: "1 MiB"},
		{value: 1468006, want: "1.4 MiB"},
		{value: 365 * 1024 * 1024, want: "365 MiB"},
		{value: 1024 * 1024 * 1024 * 1024 * 1024, want: "1 PiB"},
		{value: -1536, want: "-1.5 KiB"},
		{value: -2048, want: "-2 KiB"},
		// 単位表が尽きたら PiB のまま桁を増やし、未定義の単位へ進めない。
		{value: 1 << 60, want: "1024 PiB"},
	} {
		if got := HumanBytes(testCase.value); got != testCase.want {
			t.Fatalf("HumanBytes(%d)=%q, want %q", testCase.value, got, testCase.want)
		}
	}
}

func TestHomePathAbbreviatesOnlyPathsUnderHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, testCase := range []struct{ path, want string }{
		{path: "", want: ""},
		{path: home, want: "~"},
		{path: filepath.Join(home, "dev", "wx"), want: filepath.Join("~", "dev", "wx")},
		// home の兄弟や外側の path は書き換えない。
		{path: home + "-other", want: home + "-other"},
		{path: "/elsewhere", want: "/elsewhere"},
	} {
		if got := HomePath(testCase.path); got != testCase.want {
			t.Fatalf("HomePath(%q)=%q, want %q", testCase.path, got, testCase.want)
		}
	}
}
