package cli

import (
	"errors"
	"testing"

	"github.com/HappyOnigiri/WorktreeX/internal/daemon"
)

func TestAgentTerminatorConfirmMutationBoundariesRespectRequestID(t *testing.T) {
	for _, tt := range []struct {
		name      string
		requestID string
		wantCalls int
	}{
		{name: "without request", wantCalls: 0},
		{name: "with request", requestID: "request-1", wantCalls: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			client, _ := newBenchMutationClient(t, func(method string) (any, error) {
				if method != "ConfirmTermination" {
					return nil, errors.New("unexpected method " + method)
				}
				calls++
				return nil, nil
			})
			terminator := &agentTerminator{requestID: tt.requestID}
			terminator.confirm(client, daemon.Lease{SessionID: "session", Token: "token"})
			if calls != tt.wantCalls {
				t.Fatalf("ConfirmTermination calls=%d, want %d", calls, tt.wantCalls)
			}
		})
	}
}
