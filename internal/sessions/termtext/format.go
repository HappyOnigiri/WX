package termtext

import (
	"fmt"
	"time"
)

// 会話一覧の補助情報を短く読める形へ直す整形群。
// バイト数とホーム短縮は status 表と規則を共有するため internal/textfmt にあり、ここには置かない。

// RelativeTime は at を now からの経過時間として相対表記へ直す。
// 粒度は分・時間・日までとし、1 分未満と now より後の時刻はまとめて "just now" とする。
func RelativeTime(at, now time.Time) string {
	d := now.Sub(at)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
	}
}
