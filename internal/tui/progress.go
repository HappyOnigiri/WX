package tui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// progressFrameInterval は animation の dot 間隔。
// socket probe の間に動きを見せつつ点滅させず、test は短縮できるが production では変更しない。
var progressFrameInterval = 400 * time.Millisecond

// progressMaxDots は dot を先頭へ戻すまでの最大数。
const progressMaxDots = 3

// Progress は同期的な daemon 操作の待機中に表示する進捗行。
// 対話端末だけで描画し、pipe・log・test の出力には制御文字を出さない。
type Progress struct {
	w       io.Writer
	label   string
	trailer string
	// mu は ticker goroutine の再描画と呼び出し側の書込みを直列化する。
	// 呼び出し側は書込み前に animation 行を消す。
	mu sync.Mutex
	// drawn は最後に描いた行の表示幅。label を差し替えても前の行の末尾が残らないようにする。
	drawn    int
	frame    int
	animated bool
	stop     chan struct{}
	done     chan struct{}
}

// InteractiveOutput は command の出力先が terminal か返す。
// pipe と regular file には animation を出さない。/dev/null は見えないため特別扱いしない。
func InteractiveOutput(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// StartProgress は待機行を開始する。
// 最初の frame は return 前に描画し、command 開始直後から進捗を示す。
func StartProgress(w io.Writer, animate bool, label string) *Progress {
	p := &Progress{w: w, label: label}
	if !animate {
		return p
	}
	p.animated = true
	p.stop = make(chan struct{})
	p.done = make(chan struct{})
	p.mu.Lock()
	p.draw()
	p.mu.Unlock()
	go p.run()
	return p
}

func (p *Progress) run() {
	defer close(p.done)
	ticker := time.NewTicker(progressFrameInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			p.mu.Lock()
			p.frame++
			p.draw()
			p.mu.Unlock()
		}
	}
}

// draw は待機行を同じ位置に再描画する。短い frame が長い frame の末尾を残さないよう dot を埋める。
// p.mu を保持して呼ぶ。
func (p *Progress) draw() {
	if !p.animated {
		return
	}
	dots := strings.Repeat(".", p.frame%progressMaxDots+1)
	text := fmt.Sprintf("%s%-*s%s", p.label, progressMaxDots, dots, p.trailer)
	p.drawn = ansi.StringWidth(text)
	// 待機行は装飾であり、書込み失敗は対処できず command の終了コードも変えない。
	_, _ = fmt.Fprintf(p.w, "\r%s", text)
}

// erase は待機行を消して cursor を先頭列へ戻し、次の出力を空行から始める。
// p.mu を保持して呼ぶ。
func (p *Progress) erase() {
	if !p.animated {
		return
	}
	_, _ = fmt.Fprintf(p.w, "\r%*s\r", p.drawn, "")
	p.drawn = 0
}

// Set は待機行の表示を差し替える。trailer は dot の後ろへ置き、経過時間のように末尾で変わる値に使う。
// 描画中の goroutine と直列化するため、待機行を持つどの goroutine からでも呼べる。
// Finish 後の呼び出しは何も描かない。
func (p *Progress) Set(label, trailer string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.label == label && p.trailer == trailer {
		return
	}
	p.erase()
	p.label, p.trailer = label, trailer
	p.draw()
}

// Line は待機終了ではない message を出し、その下に待機行を戻す。
func (p *Progress) Line(text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.erase()
	_, _ = fmt.Fprintln(p.w, text)
	p.draw()
}

// Finish は待機行を消す。呼び出し側は defer と結果出力前の両方で呼ぶため、二度目を許容する。
// 同時呼び出しは stop の二重 close になるため、Set と違い 1 つの goroutine から呼ぶ。
func (p *Progress) Finish() {
	p.mu.Lock()
	animated := p.animated
	p.mu.Unlock()
	if !animated {
		return
	}
	close(p.stop)
	<-p.done
	p.mu.Lock()
	p.erase()
	p.animated = false
	p.mu.Unlock()
}
