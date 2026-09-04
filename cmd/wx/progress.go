package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// progressFrameInterval paces the animated dots. Slow enough not to flicker,
// fast enough that the line visibly moves between two socket probes. It is a
// variable so tests can shorten it; production never changes it.
var progressFrameInterval = 400 * time.Millisecond

// progressMaxDots is how far the dots grow before they start over at one.
const progressMaxDots = 3

// progress is the "stopping..." line the synchronous daemon commands show
// while they wait. The wait is dominated by the daemon's idle gate, which is
// silence from the daemon's side, and a command that prints nothing at all
// until it succeeds reads as a hang.
//
// The animation is for an interactive terminal only: it is carriage returns
// and padding, which is noise once the output is a pipe, a log file or a test.
// Non-interactive output therefore shows no waiting line at all, and the type
// still carries the finished messages so the callers do not have to branch.
type progress struct {
	w     io.Writer
	label string
	// mu serialises the ticker goroutine's redraws against the caller's own
	// writes, which have to erase the animated line before they use it.
	mu       sync.Mutex
	frame    int
	animated bool
	stop     chan struct{}
	done     chan struct{}
}

// interactiveOutput reports whether the command's output goes to a terminal.
// A pipe and a regular file both answer no, which is what keeps the animation
// out of everything a script, a log or a test reads back. /dev/null is a
// character device and answers yes, which costs a few carriage returns that
// nobody sees and is not worth a syscall to tell apart.
func interactiveOutput(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// startProgress begins the waiting line. The first frame is drawn before the
// call returns, so the feedback is there from the moment the command starts
// rather than one frame interval later.
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

// draw rewrites the waiting line in place. The dots are padded to their full
// width so a shorter frame cannot leave the tail of a longer one behind.
// p.mu must be held.
func (p *progress) draw() {
	if !p.animated {
		return
	}
	dots := strings.Repeat(".", p.frame%progressMaxDots+1)
	// The waiting line is decoration; a write failure on it is not actionable
	// and must not change the command's exit status.
	_, _ = fmt.Fprintf(p.w, "\r%s%-*s", p.label, progressMaxDots, dots)
}

// erase clears the waiting line and returns the cursor to column zero, so
// whatever is written next starts on a blank line. p.mu must be held.
func (p *progress) erase() {
	if !p.animated {
		return
	}
	_, _ = fmt.Fprintf(p.w, "\r%*s\r", len(p.label)+progressMaxDots, "")
}

// line prints a message that is not the end of the wait, and puts the waiting
// line back underneath it.
func (p *progress) line(text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.erase()
	_, _ = fmt.Fprintln(p.w, text)
	p.draw()
}

// finish takes the waiting line down. Callers defer it and also call it before
// writing the outcome, so it has to tolerate the second call; it is not safe
// against two goroutines, which the commands never do.
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
