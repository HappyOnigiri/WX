// Package textfmt は表示用の整形のうち、複数の画面で同じ規則を守る必要があるものを持つ。
// status 表と会話 picker が別実装を持つと、桁分けやホーム短縮の規則が片側だけ変わって黙って食い違う。
package textfmt

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

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
