package workspace

// このファイルは prepare command の process 寿命と出力回収を扱う。
// prepare の timeout/cancel を、出力 pipe を継承した descendant の生存から切り離すのが役目である。

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// prepareOutputDrainGrace は command 終了後に診断出力の回収へ与える猶予である。
// 出力 pipe を継承した descendant が生き残っても、prepare の timeout/cancel をこの猶予までで返す。
const prepareOutputDrainGrace = 500 * time.Millisecond

// markCaptureIncomplete は猶予内に回収し切れなかった出力があることを記録する。
func (d *prepareDiagnostic) markCaptureIncomplete() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.truncated["stdout"] = true
	d.truncated["stderr"] = true
}

// prepareOutputCapture は stdout/stderr を親が所有する pipe で受け取り、回収を cmd.Wait から切り離す。
// os/exec は *os.File をそのまま子の fd に複製するため、copy goroutine を作らず Wait が EOF を待たなくなる。
// 結果として返却までの時間は command process の終了だけで決まり、pipe を保持する descendant に依存しない。
type prepareOutputCapture struct {
	writers []*os.File
	readers []*os.File
	drained chan struct{}
}

func newPrepareOutputCapture(cmd *exec.Cmd, diagnostic *prepareDiagnostic) (*prepareOutputCapture, error) {
	capture := &prepareOutputCapture{drained: make(chan struct{})}
	var copies sync.WaitGroup
	for _, stream := range []string{"stdout", "stderr"} {
		reader, writer, err := os.Pipe()
		if err != nil {
			capture.closeWriters()
			capture.closeReaders()
			close(capture.drained)
			return nil, fmt.Errorf("create prepare output pipe: %w", err)
		}
		capture.readers = append(capture.readers, reader)
		capture.writers = append(capture.writers, writer)
		if stream == "stdout" {
			cmd.Stdout = writer
		} else {
			cmd.Stderr = writer
		}
		copies.Add(1)
		go func() {
			defer copies.Done()
			_, _ = io.Copy(prepareDiagnosticWriter{diagnostic: diagnostic, stream: stream}, reader)
		}()
	}
	go func() {
		copies.Wait()
		close(capture.drained)
	}()
	return capture, nil
}

// closeWriters は親が持つ write 端を閉じる。閉じ忘れると子孫が終了しても EOF が届かない。
func (c *prepareOutputCapture) closeWriters() {
	for _, writer := range c.writers {
		_ = writer.Close()
	}
	c.writers = nil
}

func (c *prepareOutputCapture) closeReaders() {
	for _, reader := range c.readers {
		_ = reader.Close()
	}
	c.readers = nil
}

// finish は残りの出力を grace まで回収し、回収し切れたかどうかを返す。
// 猶予を超えたら read 端を閉じる。読み取り中の goroutine はエラーで解けるので、待ち続けずに済む。
func (c *prepareOutputCapture) finish(grace time.Duration) bool {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	complete := true
	select {
	case <-c.drained:
	case <-timer.C:
		complete = false
	}
	c.closeReaders()
	<-c.drained
	return complete
}

// configurePrepareProcessGroup は timeout/cancel で command の子孫ごと終了させる。
// Setpgid で専用の process group を作らないと -pid が wx 自身の group を指し得るため、両者は対で必要になる。
func configurePrepareProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			// group が既に無い場合や Setpgid が効かなかった場合は、従来どおり command process だけを落とす。
			return cmd.Process.Kill()
		}
		return nil
	}
}
