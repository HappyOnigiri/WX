package main

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// displayKeyWidth は表示行の key 列の最小幅。
// 入れ子 key は長くなり得るため、実際の幅は payload ごとに計算する。
const displayKeyWidth = 20

// printDisplay は RPC の表示用 payload を人間向けに整形する。
// 入れ子の値は dotted key に展開し、スクリプト向けの --json は別経路で出力する。
func printDisplay(w io.Writer, payload map[string]any) {
	pairs := appendDisplayPairs(nil, "", payload)
	width := displayKeyWidth
	for _, pair := range pairs {
		if len(pair.key) > width {
			width = len(pair.key)
		}
	}
	for _, pair := range pairs {
		// 表示は stdout に出す。書込み失敗は対処できず、command の終了コードも変えない。
		_, _ = fmt.Fprintf(w, "%-*s %s\n", width, pair.key, pair.value)
	}
}

type displayPair struct{ key, value string }

// appendDisplayPairs は map key を sort して depth-first に値をたどる。
// Go の map 走査順にかかわらず出力順を安定させる。
func appendDisplayPairs(pairs []displayPair, prefix string, value any) []displayPair {
	switch typed := value.(type) {
	case map[string]any:
		if len(typed) == 0 {
			return append(pairs, displayPair{displayKey(prefix), "(none)"})
		}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			child := key
			if prefix != "" {
				child = prefix + "." + key
			}
			pairs = appendDisplayPairs(pairs, child, typed[key])
		}
		return pairs
	case []any:
		if len(typed) == 0 {
			return append(pairs, displayPair{displayKey(prefix), "(none)"})
		}
		if scalars, ok := scalarElements(typed); ok {
			return append(pairs, displayPair{displayKey(prefix), strings.Join(scalars, ", ")})
		}
		for i, element := range typed {
			pairs = appendDisplayPairs(pairs, fmt.Sprintf("%s[%d]", prefix, i), element)
		}
		return pairs
	case nil:
		return append(pairs, displayPair{displayKey(prefix), "(none)"})
	default:
		return append(pairs, displayPair{displayKey(prefix), displayScalar(value)})
	}
}

// scalarElements は全要素が scalar か調べる。
// string・number の list は一行に収め、map・入れ子 list を含む場合は index 付きで要素ごとに表示する。
func scalarElements(elements []any) ([]string, bool) {
	out := make([]string, 0, len(elements))
	for _, element := range elements {
		switch element.(type) {
		case map[string]any, []any, nil:
			return nil, false
		}
		out = append(out, displayScalar(element))
	}
	return out, true
}

// displayScalar は JSON scalar を整形する。
// number は float64 で届くため、整数を指数表記せず表示する。
func displayScalar(value any) string {
	if number, ok := value.(float64); ok {
		return strconv.FormatFloat(number, 'f', -1, 64)
	}
	return fmt.Sprint(value)
}

// displayKey は空の key を補う。
// 呼び出し側は top-level map を渡すため、これは object 以外の payload 用の fallback である。
func displayKey(prefix string) string {
	if prefix == "" {
		return "value"
	}
	return prefix
}
