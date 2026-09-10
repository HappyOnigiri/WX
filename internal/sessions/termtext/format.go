package termtext

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// 会話一覧の補助情報を短く読める形へ直す整形群。
// 桁分けとホーム短縮の規則は cmd/wx の status 表と同じだが、status の出力形状は表の規約に縛られるため共有しない。

// HumanBytes はバイト数を 2 進接頭辞付きの表記へ直す。
// 単位が B のときと割った結果が整数のときは小数を付けず、それ以外は小数 2 桁にして末尾の 0 を落とす。
func HumanBytes(value int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	negative := value < 0
	n := float64(value)
	if negative {
		n = -n
	}
	unit := 0
	for n >= 1024 && unit < len(units)-1 {
		n /= 1024
		unit++
	}
	var number string
	if unit == 0 || n == math.Trunc(n) {
		number = strconv.FormatFloat(n, 'f', 0, 64)
	} else {
		number = strings.TrimRight(strings.TrimRight(strconv.FormatFloat(n, 'f', 2, 64), "0"), ".")
	}
	if negative {
		number = "-" + number
	}
	return number + " " + units[unit]
}

// RelativeTime は at を now からの経過時間として日本語の相対表記へ直す。
// 粒度は分・時間・日までとし、1 分未満と now より後の時刻はまとめて「たった今」とする。
func RelativeTime(at, now time.Time) string {
	d := now.Sub(at)
	switch {
	case d < time.Minute:
		return "たった今"
	case d < time.Hour:
		return fmt.Sprintf("%d分前", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d時間前", int(d/time.Hour))
	default:
		return fmt.Sprintf("%d日前", int(d/(24*time.Hour)))
	}
}

// HomePath はホーム配下の path だけを `~` 始まりへ短縮する。
// ホームを決められないときと配下でないときは、判別を誤らせないよう元の path をそのまま返す。
func HomePath(path string) string {
	if path == "" {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	home = filepath.Clean(home)
	path = filepath.Clean(path)
	if path == home {
		return "~"
	}
	rel, err := filepath.Rel(home, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	return filepath.Join("~", rel)
}
