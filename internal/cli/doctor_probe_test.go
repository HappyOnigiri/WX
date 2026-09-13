package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/rpc"
)

// statusHandler は Status の応答だけを差し替える handler である。
// probe の対象選びは Status の workspace_details だけで決まるため、貸出以降は呼ばれない。
type statusHandler struct {
	status map[string]any
}

func (h *statusHandler) Handle(_ context.Context, method string, _ json.RawMessage) (any, error) {
	if method == "Status" {
		return h.status, nil
	}
	return map[string]bool{"ok": true}, nil
}

func TestProbeWorkspacesSkipsPoliciesThatRefuseLeases(t *testing.T) {
	t.Parallel()
	temp, err := os.MkdirTemp("/tmp", "wx-cli-probe-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(temp) })
	socket := filepath.Join(temp, "wxd.sock")
	handler := &statusHandler{status: map[string]any{
		"workspace_details": []map[string]any{
			{"root": "/repo/hot", "policy": "hot"},
			{"root": "/repo/cold", "policy": "cold"},
			// ask は端末での選択を要するため、非対話の probe では必ず貸出を断られる。
			{"root": "/repo/ask", "policy": "ask"},
			{"root": "/repo/off", "policy": "off"},
			{"root": "", "policy": "hot"},
		},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	server := &rpc.Server{Socket: socket, Handler: handler}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	waitForSocket(t, socket, done)
	client := Client{RPC: rpc.Client{Socket: socket, Timeout: time.Second}, Config: config.Defaults()}
	roots, err := client.probeWorkspaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/repo/cold", "/repo/hot"}; !reflect.DeepEqual(roots, want) {
		t.Fatalf("probeWorkspaces=%v want=%v", roots, want)
	}
}
