package termtext

import (
	"fmt"
	"time"
)

// 会話一覧の補助情報を短く読める形へ直す整形群。
// バイト数とホーム短縮は status 表と規則を共有するため internal/textfmt にあり、ここには置かない。

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
