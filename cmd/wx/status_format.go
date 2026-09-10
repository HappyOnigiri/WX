package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/HappyOnigiri/WX/internal/textfmt"
)

// 表示に使うタイムゾーン。テストが固定の地域時刻を検査するための差し替え点である。
// グローバルなtime.Localを書き換えると、並行するgoroutineのtime.Nowと競合する。
var statusDisplayLocation = time.Local

func statusPathWithin(path, root string) bool {
	if path == "" || root == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func statusDash(value string) string {
	if value == "" {
		return "—"
	}
	return value
}

func statusHomeValue(object map[string]any, key string) string {
	value, ok := object[key]
	if !ok {
		return "—"
	}
	return textfmt.HomePath(statusRawValue(value))
}

func statusLocalDate(raw string) string {
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return "—"
	}
	return parsed.In(statusDisplayLocation).Format("01/02 15:04")
}

func statusZoneLabel() string {
	name, _ := time.Now().In(statusDisplayLocation).Zone()
	if name == "" {
		return "LOCAL"
	}
	return name
}

func statusObjectList(value any) []map[string]any {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if object, ok := item.(map[string]any); ok {
			out = append(out, object)
		}
	}
	return out
}

func statusObjectsSortedBy(objects []map[string]any, key string) []map[string]any {
	if len(objects) < 2 {
		return objects
	}
	out := append([]map[string]any(nil), objects...)
	sort.SliceStable(out, func(i, j int) bool {
		left := statusValueRaw(out[i], key)
		right := statusValueRaw(out[j], key)
		if left == right {
			return statusValueRaw(out[i], "id") < statusValueRaw(out[j], "id")
		}
		return left < right
	})
	return out
}

func appendStatusUnknown(pairs []displayPair, prefix string, object map[string]any, known map[string]bool) []displayPair {
	keys := make([]string, 0)
	for key := range object {
		if !known[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		pairs = appendDisplayPairs(pairs, prefix+"."+key, object[key])
	}
	return pairs
}

func statusRawString(object map[string]any, key string) (string, bool) {
	value, ok := object[key]
	if !ok {
		return "", false
	}
	switch typed := value.(type) {
	case string:
		return typed, true
	case nil:
		return "", true
	default:
		return displayScalar(typed), true
	}
}

func statusValueRaw(object map[string]any, key string) string {
	value, ok := object[key]
	if !ok {
		return ""
	}
	return statusRawValue(value)
}

func statusValue(object map[string]any, key string) string {
	value, ok := object[key]
	if !ok {
		return "—"
	}
	return statusRawValue(value)
}

func statusRawValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return "(null)"
	case string:
		if typed == "" {
			return "(empty)"
		}
		return typed
	case []any:
		if len(typed) == 0 {
			return "(none)"
		}
	case map[string]any:
		if len(typed) == 0 {
			return "(none)"
		}
	}
	return displayScalar(value)
}

func statusCountOrDash(object map[string]any, key string) string {
	if value, ok := statusInt(object, key); ok {
		return strconv.FormatInt(value, 10)
	}
	return "—"
}

func statusInt(object map[string]any, key string) (int64, bool) {
	value, ok := object[key]
	if !ok || value == nil {
		return 0, false
	}
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int8:
		return int64(typed), true
	case int16:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case uint:
		return int64(typed), true
	case uint8:
		return int64(typed), true
	case uint16:
		return int64(typed), true
	case uint32:
		return int64(typed), true
	case uint64:
		if typed > math.MaxInt64 {
			return 0, false
		}
		return int64(typed), true
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || typed != math.Trunc(typed) || typed > math.MaxInt64 || typed < math.MinInt64 {
			return 0, false
		}
		return int64(typed), true
	case float32:
		f := float64(typed)
		if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) || f > math.MaxInt64 || f < math.MinInt64 {
			return 0, false
		}
		return int64(f), true
	case json.Number:
		n, err := strconv.ParseInt(string(typed), 10, 64)
		return n, err == nil
	default:
		return 0, false
	}
}

func statusBool(object map[string]any, key string) (bool, bool) {
	value, ok := object[key]
	if !ok {
		return false, false
	}
	typed, ok := value.(bool)
	return typed, ok
}

func statusExactBytes(object map[string]any, key string) string {
	value, ok := statusInt(object, key)
	if !ok {
		return "—"
	}
	return strconv.FormatInt(value, 10) + " bytes"
}

func formatDurationSeconds(seconds int64) string {
	return strconv.FormatInt(seconds, 10) + "s (" + humanDurationSeconds(seconds) + ")"
}

func humanDurationSeconds(seconds int64) string {
	if seconds == 0 {
		return "0s"
	}
	negative := seconds < 0
	var value uint64
	if negative {
		// math.MinInt64 の絶対値は int64 に収まらないため、オフセットして uint64 の大きさを求める。
		value = uint64(-(seconds + 1)) + 1
	} else {
		value = uint64(seconds)
	}
	parts := make([]string, 0, 4)
	for _, unit := range []struct {
		seconds uint64
		name    string
	}{{86400, "day"}, {3600, "h"}, {60, "m"}, {1, "s"}} {
		if value >= unit.seconds {
			count := value / unit.seconds
			value %= unit.seconds
			if unit.name == "day" {
				name := "day"
				if count != 1 {
					name = "days"
				}
				parts = append(parts, fmt.Sprintf("%d %s", count, name))
			} else {
				parts = append(parts, fmt.Sprintf("%d%s", count, unit.name))
			}
		}
	}
	result := strings.Join(parts, " ")
	if negative {
		return "-" + result
	}
	return result
}

func formatRetentionValue(value any) string {
	if seconds, ok := statusInt(map[string]any{"value": value}, "value"); ok {
		return formatDurationSeconds(seconds)
	}
	return statusRawValue(value)
}

func statusQuarantineReason(item map[string]any) string {
	value, ok := item["failure_code"]
	if !ok {
		return "(unset)"
	}
	return statusRawValue(value)
}

func writeStatusLine(w io.Writer, line string) {
	_, _ = fmt.Fprintln(w, line)
}

func writeStatusField(w io.Writer, label, value string) {
	if value == "" {
		value = "(empty)"
	}
	// 長い error・path・opaque ID は値を失わないよう継続行へ送り、短い値は 1 行に収める。
	if strings.Contains(value, "\n") || utf8.RuneCountInString(value) > 120 {
		writeStatusLine(w, label+":")
		for _, line := range strings.Split(value, "\n") {
			writeStatusLine(w, "    "+line)
		}
		return
	}
	writeStatusLine(w, label+": "+value)
}

func writeStatusTable(w io.Writer, headers []string, rows [][]string) {
	widths := make([]int, len(headers))
	for index, header := range headers {
		widths[index] = utf8.RuneCountInString(header)
	}
	for _, row := range rows {
		for index := range headers {
			if index < len(row) {
				if width := utf8.RuneCountInString(row[index]); width > widths[index] {
					widths[index] = width
				}
			}
		}
	}
	writeRow := func(row []string) {
		var line strings.Builder
		for index, header := range headers {
			value := header
			if index < len(row) {
				value = row[index]
			}
			if index > 0 {
				line.WriteByte(' ')
			}
			line.WriteString(value)
			padding := widths[index] - utf8.RuneCountInString(value)
			if index < len(headers)-1 {
				line.WriteString(strings.Repeat(" ", padding))
			}
		}
		writeStatusLine(w, line.String())
	}
	writeRow(nil)
	for _, row := range rows {
		writeRow(row)
	}
}
