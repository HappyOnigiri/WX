module github.com/HappyOnigiri/WorktreeX

go 1.27.1

require (
	charm.land/bubbletea/v2 v2.0.9
	github.com/BurntSushi/toml v1.6.0
	github.com/charmbracelet/x/ansi v0.11.8
	github.com/nicksnyder/go-i18n/v2 v2.6.1
	github.com/spf13/pflag v1.0.10
	golang.org/x/sys v0.47.0
	golang.org/x/text v0.32.0
	gopkg.in/yaml.v3 v3.0.1
	modernc.org/sqlite v1.57.0
)

require (
	github.com/charmbracelet/colorprofile v0.4.3 // indirect
	// bubbletea が要求する版より新しく固定する。古い版は全角文字を含む行を
	// セル差分で更新し、hard tab や EL が全角文字の後半セルへ落ちて字が壊れる。
	github.com/charmbracelet/ultraviolet v0.0.0-20260811164956-006e29f97886 // indirect
	github.com/charmbracelet/x/term v0.2.2 // indirect
	github.com/charmbracelet/x/termios v0.1.1 // indirect
	github.com/charmbracelet/x/windows v0.2.2 // indirect
	github.com/clipperhouse/displaywidth v0.11.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/lucasb-eyer/go-colorful v1.4.1 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/mattn/go-runewidth v0.0.24 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/xo/terminfo v0.0.0-20220910002029-abceb7e1c41e // indirect
	golang.org/x/sync v0.22.0 // indirect
	modernc.org/libc v1.74.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
