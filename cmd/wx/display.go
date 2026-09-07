package main

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/HappyOnigiri/WX/internal/config"
)

const slotRowFormat = "%-8s %-12s %-16s %-8s %-8s %-11s %8s  %s\n"

func slotField(row map[string]any, key string) string {
	if value, ok := row[key].(string); ok && value != "" {
		return value
	}
	return "-"
}

// slotRepositories は行のリポジトリを basename のカンマ区切りで返す。
// フルパスは --json 側に残し、表では列幅を basename に抑える。
func slotRepositories(row map[string]any) string {
	paths, _ := row["repositories"].([]any)
	names := make([]string, 0, len(paths))
	for _, path := range paths {
		if text, ok := path.(string); ok && text != "" {
			names = append(names, filepath.Base(text))
		}
	}
	if len(names) == 0 {
		return "-"
	}
	return strings.Join(names, ",")
}

// slotCopyMode は方式を出し、まだ決まらない行には copy_mode の代わりに理由（pending・unsupported）を出す。
// 空欄にすると測定前と情報のない行を見分けられない。
func slotCopyMode(row map[string]any) string {
	if mode, ok := row["copy_mode"].(string); ok && mode != "" {
		return mode
	}
	return slotField(row, "measurement")
}

// slotSizeMB は slot が専有する bytes を MB へ切り上げ、3 桁区切りで返す。
// main worktree と共有している block を除いた量なので、slot を消して解放される見込みの大きさにあたる。
// 測定前の行は 0 ではなく - と表示する。
func slotSizeMB(row map[string]any) string {
	if _, measured := row["measured_at"].(string); !measured {
		return "-"
	}
	const megabyte = 1 << 20
	exclusive, _ := row["exclusive_bytes"].(float64)
	if exclusive < 0 {
		exclusive = 0
	}
	return formatThousands((int64(exclusive) + megabyte - 1) / megabyte)
}

// formatThousands は負でない整数を 3 桁ごとに , で区切る。
func formatThousands(value int64) string {
	digits := strconv.FormatInt(value, 10)
	var out strings.Builder
	for i := range len(digits) {
		if i > 0 && (len(digits)-i)%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteByte(digits[i])
	}
	return out.String()
}

// replyInt はライフサイクル応答から数値を読む。JSON 数値は float64 になり、旧 daemon の欠落項目は 0 とする。
func replyInt(reply map[string]any, key string) int {
	value, _ := reply[key].(float64)
	return int(value)
}

// lifecycleConflict は反対の要求を拒否した daemon が実行中の操作を示す。
// 送信済みの signal は取り消せないため、要求した状態を待つだけでは期限を消費する。
func lifecycleConflict(reply map[string]any, wanted string) string {
	if conflict, _ := reply["conflict"].(bool); !conflict {
		return ""
	}
	if stopping, _ := reply["stop_pending"].(bool); stopping && wanted != "stop" {
		return "the daemon is already stopping; wait for it to exit and run wx daemon start"
	}
	if restarting, _ := reply["restart_pending"].(bool); restarting && wanted != "restart" {
		return "the daemon is already restarting; run the command again once the replacement is up"
	}
	return ""
}

// gateWaitReason は要求受理時のスナップショットに基づき、daemon が待っていた理由を説明する。
// 受理後の状態は問い合わせず、待機中の RPC でゲートの判断材料を増やさない。
func gateWaitReason(reply map[string]any) string {
	if jobs := replyInt(reply, "queued_jobs"); jobs > 0 {
		return fmt.Sprintf("%d job(s) were still queued when the request was accepted; the daemon waits for them to finish", jobs)
	}
	if inflight := replyInt(reply, "inflight_requests"); inflight > 0 {
		return fmt.Sprintf("%d other request(s) were still in flight when the request was accepted", inflight)
	}
	reason := "the daemon was idle when the request was accepted, so a long-running request or job arrived after that"
	logPath, err := config.LogPath()
	if err != nil {
		return reason + "; check the daemon log for the requests and jobs that followed"
	}
	return fmt.Sprintf("%s; check %s for the requests and jobs that followed", reason, logPath)
}

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
