package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// progressFrameInterval は animation の dot 間隔。
// socket probe の間に動きを見せつつ点滅させず、test は短縮できるが production では変更しない。
var progressFrameInterval = 400 * time.Millisecond

// progressMaxDots は dot を先頭へ戻すまでの最大数。
const progressMaxDots = 3

// progress は同期的な daemon 操作の待機中に表示する進捗行。
// 対話端末だけで描画し、pipe・log・test の出力には制御文字を出さない。
type progress struct {
	w     io.Writer
	label string
	// mu は ticker goroutine の再描画と呼び出し側の書込みを直列化する。
	// 呼び出し側は書込み前に animation 行を消す。
	mu       sync.Mutex
	frame    int
	animated bool
	stop     chan struct{}
	done     chan struct{}
}

// interactiveOutput は command の出力先が terminal か返す。
// pipe と regular file には animation を出さない。/dev/null は見えないため特別扱いしない。
func interactiveOutput(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// startProgress は待機行を開始する。
// 最初の frame は return 前に描画し、command 開始直後から進捗を示す。
func startProgress(w io.Writer, animate bool, label string) *progress {
	p := &progress{w: w, label: label}
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

func (p *progress) run() {
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
func (p *progress) draw() {
	if !p.animated {
		return
	}
	dots := strings.Repeat(".", p.frame%progressMaxDots+1)
	// 待機行は装飾であり、書込み失敗は対処できず command の終了コードも変えない。
	_, _ = fmt.Fprintf(p.w, "\r%s%-*s", p.label, progressMaxDots, dots)
}

// erase は待機行を消して cursor を先頭列へ戻し、次の出力を空行から始める。
// p.mu を保持して呼ぶ。
func (p *progress) erase() {
	if !p.animated {
		return
	}
	_, _ = fmt.Fprintf(p.w, "\r%*s\r", len(p.label)+progressMaxDots, "")
}

// line は待機終了ではない message を出し、その下に待機行を戻す。
func (p *progress) line(text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.erase()
	_, _ = fmt.Fprintln(p.w, text)
	p.draw()
}

// finish は待機行を消す。呼び出し側は defer と結果出力前の両方で呼ぶため、二度目を許容する。
// command は複数 goroutine から呼ばない。
func (p *progress) finish() {
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
