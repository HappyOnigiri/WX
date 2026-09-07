package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// FuzzDurationYAML は Duration の node 解釈だけを対象にする。
// YAML 文書全体を入力にすると anchor 展開に時間を取られ、Duration のパースへ到達する実行数が落ちる。
func FuzzDurationYAML(f *testing.F) {
	for _, seed := range []string{
		"10m", "0s", "-1h30m", "2562047h47m16.854775807s", "-2562047h47m16.854775807s",
		"1h2m3s4ms5us6ns", "0.5h", ".5s", "1e3s", "", " 10m", "10m ", "not-a-duration",
		"10", "1d", "+5m", "--5m", "9223372036854775808ns",
	} {
		f.Add(int(yaml.ScalarNode), seed)
	}
	for _, kind := range []yaml.Kind{yaml.MappingNode, yaml.SequenceNode, yaml.AliasNode, yaml.DocumentNode} {
		f.Add(int(kind), "10m")
	}
	f.Fuzz(func(t *testing.T, kind int, value string) {
		var duration Duration
		err := duration.UnmarshalYAML(&yaml.Node{Kind: yaml.Kind(kind), Tag: "!!str", Value: value})
		if yaml.Kind(kind) != yaml.ScalarNode {
			if err == nil {
				t.Fatalf("non-scalar node kind %d accepted value %q", kind, value)
			}
			return
		}
		if err != nil {
			if duration.Duration != 0 {
				t.Fatalf("rejected %q but stored %v", value, duration.Duration)
			}
			return
		}
		// 受理した値は MarshalYAML の表記から同じ Duration に戻る必要がある。
		// 戻らなければ設定を書き戻すたびに期間が変わる。
		encoded, err := duration.MarshalYAML()
		if err != nil {
			t.Fatal(err)
		}
		text, ok := encoded.(string)
		if !ok {
			t.Fatalf("MarshalYAML returned %T, want string", encoded)
		}
		var roundTrip Duration
		if err := roundTrip.UnmarshalYAML(&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: text}); err != nil {
			t.Fatalf("re-parse of %q (from %q): %v", text, value, err)
		}
		if roundTrip != duration {
			t.Fatalf("round trip=%v, want %v", roundTrip.Duration, duration.Duration)
		}
		if parsed, err := time.ParseDuration(text); err != nil || parsed != duration.Duration {
			t.Fatalf("ParseDuration(%q)=%v, %v; want %v", text, parsed, err, duration.Duration)
		}
	})
}
