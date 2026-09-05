package cli

import (
	"context"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/HappyOnigiri/WX/internal/daemon"
)

// terminationEnvelope は heartbeat と agent 登録の応答に載る終了要求である。
// 旧 daemon は項目を返さないため、欠落は「要求なし」として扱う。
type terminationEnvelope struct {
	Terminate *terminationRequest `json:"terminate"`
}

type terminationRequest struct {
	RequestID string `json:"request_id"`
	Deadline  string `json:"deadline"`
}

// agentTerminator は wx clean の終了要求を 1 度だけ agent へ伝え、停止確認の応答先を覚える。
// signal を送るのは client であり、daemon は記録された PID へ signal を送らない。
type agentTerminator struct {
	mu        sync.Mutex
	cmd       *exec.Cmd
	requestID string
	signaled  bool
}

// adopt は signal 送信先の process を登録する。登録前に届いた要求は、ここで初めて配送される。
func (t *agentTerminator) adopt(cmd *exec.Cmd) {
	t.mu.Lock()
	t.cmd = cmd
	pending := t.requestID != "" && !t.signaled
	t.mu.Unlock()
	if pending {
		t.signal()
	}
}

// request は受け取った終了要求を記録し、process group へ SIGTERM を一度だけ送る。
func (t *agentTerminator) request(request *terminationRequest) {
	if request == nil || request.RequestID == "" {
		return
	}
	t.mu.Lock()
	if t.requestID == "" {
		t.requestID = request.RequestID
	}
	t.mu.Unlock()
	t.signal()
}

func (t *agentTerminator) signal() {
	t.mu.Lock()
	if t.signaled || t.cmd == nil || t.cmd.Process == nil {
		t.mu.Unlock()
		return
	}
	t.signaled = true
	cmd := t.cmd
	t.mu.Unlock()
	forwardAgentSignal(cmd, syscall.SIGTERM)
}

func (t *agentTerminator) requested() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.requestID != ""
}

// confirm は agent の停止を確認した後に daemon へ応答する。応答がなければ、その要求は期限切れで失敗として閉じられる。
func (t *agentTerminator) confirm(c Client, lease daemon.Lease) {
	t.mu.Lock()
	requestID := t.requestID
	t.mu.Unlock()
	if requestID == "" {
		return
	}
	confirmCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = c.RPC.CallWithKey(confirmCtx, "ConfirmTermination", "terminate:"+lease.SessionID+":"+requestID,
		map[string]any{"session_id": lease.SessionID, "token": lease.Token, "request_id": requestID}, nil)
}

// heartbeatInterval は heartbeat の送信間隔で、終了要求を受け取る間隔でもある。
// daemon が待つ猶予（30秒）内に signal を送って停止を確認できる長さにする。
const heartbeatInterval = 10 * time.Second

// watchTermination は heartbeat を送り続け、応答に載る終了要求を agent へ伝える。
// binary 置換後の再起動中に heartbeat が失敗しても、ConnectRetry で短い未待受期間を吸収する。
func (c Client) watchTermination(ctx context.Context, lease daemon.Lease, terminator *agentTerminator, done <-chan struct{}) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			var reply terminationEnvelope
			heartbeatCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := c.RPC.Call(heartbeatCtx, "Heartbeat", map[string]string{"session_id": lease.SessionID, "token": lease.Token}, &reply)
			cancel()
			if err == nil {
				terminator.request(reply.Terminate)
			}
		case <-done:
			return
		case <-ctx.Done():
			return
		}
	}
}
