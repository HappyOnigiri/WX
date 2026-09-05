package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/sessions"
	"github.com/HappyOnigiri/WX/internal/sessions/identity"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestResumeArgsBuildsAgentSpecificResumeCommands(t *testing.T) {
	rest := []string{"--model", "opus"}
	tests := []struct {
		name  string
		agent string
		id    string
		path  string
		want  []string
	}{
		{name: "claude native id", agent: "claude", id: "claude-session", path: "/tmp/slot", want: []string{"--resume", "claude-session", "--model", "opus"}},
		{name: "codex native id with cd", agent: "codex", id: "codex-session", path: "/tmp/slot", want: []string{"resume", "--cd", "/tmp/slot", "codex-session", "--model", "opus"}},
		{name: "claude without native id", agent: "claude", path: "/tmp/slot", want: rest},
		{name: "codex without native id", agent: "codex", path: "/tmp/slot", want: rest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := resumeArgs(test.agent, test.id, test.path, rest)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("resumeArgs(%q, %q, %q)=%v, want %v", test.agent, test.id, test.path, got, test.want)
			}
		})
	}
}

func TestResolveResumeHandlesNoneAndExplicitWXSession(t *testing.T) {
	client := Client{Config: config.Defaults()}
	ctx := context.Background()

	got, found, err := client.resolveResume(ctx, "claude", "/workspace", resumeIntent{Kind: resumeIntentNone}, "")
	if err != nil || found || !reflect.DeepEqual(got, resumeTarget{}) {
		t.Fatalf("none resume target=%+v found=%v err=%v", got, found, err)
	}

	got, found, err = client.resolveResume(ctx, "codex", "/workspace", resumeIntent{Kind: resumeIntentNone}, "wx-session")
	want := resumeTarget{Agent: "codex", WXSessionID: "wx-session"}
	if err != nil || !found || !reflect.DeepEqual(got, want) {
		t.Fatalf("explicit resume target=%+v found=%v err=%v, want %+v", got, found, err, want)
	}
}

func TestLookupResumeUsesDaemonReverseMapping(t *testing.T) {
	isolatedResumeHome(t)
	handler := &resumeTestRPCHandler{
		resumeStatus: resumeStatus{WXSessionID: "wx-session", Agent: "claude", AgentSessionID: "native-session"},
	}
	client := serveResumeTestRPC(t, handler)

	got, found, err := client.lookupResume(context.Background(), "claude", "native-session")
	if err != nil {
		t.Fatal(err)
	}
	want := resumeTarget{Agent: "claude", AgentSessionID: "native-session", WXSessionID: "wx-session"}
	if !found || !reflect.DeepEqual(got, want) {
		t.Fatalf("daemon reverse mapping target=%+v found=%v, want %+v", got, found, want)
	}
	if methods := handler.calledMethods(); !reflect.DeepEqual(methods, []string{"ResumeStatus"}) {
		t.Fatalf("lookup RPC methods=%v, want ResumeStatus only", methods)
	}
}

func TestLookupResumeReadsHistoryWhenDaemonHasNoMapping(t *testing.T) {
	home := isolatedResumeHome(t)
	client := serveResumeTestRPC(t, &resumeTestRPCHandler{resumeErr: errors.New("no rows")})
	id := "11111111-1111-4111-8111-111111111111"
	root := filepath.Join(home, ".claude", "projects", "workspace")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, id+".jsonl")
	for _, cwd := range []string{"/workspace/first", "/workspace/changed"} {
		row, err := json.Marshal(map[string]any{"type": "user", "sessionId": id, "cwd": cwd, "message": map[string]any{"role": "user", "content": "Resume title"}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(row, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		got, found, err := client.lookupResume(context.Background(), "claude", id)
		if err != nil || !found || got.CWD != cwd || got.AgentSessionID != id {
			t.Fatalf("lookup=%+v found=%v err=%v", got, found, err)
		}
	}
}

func TestLookupResumePassesThroughUnknownAndReportsConnectionErrors(t *testing.T) {
	isolatedResumeHome(t)
	unknownHandler := &resumeTestRPCHandler{resumeErr: errors.New("no rows")}
	client := serveResumeTestRPC(t, unknownHandler)
	got, found, err := client.lookupResume(context.Background(), "codex", "unknown-session")
	if err != nil || found || !reflect.DeepEqual(got, resumeTarget{}) {
		t.Fatalf("unknown reverse mapping target=%+v found=%v err=%v", got, found, err)
	}

	missing := Client{
		RPC:    rpc.Client{Socket: filepath.Join(t.TempDir(), "missing.sock"), Timeout: 100 * time.Millisecond},
		Config: config.Defaults(),
	}
	got, found, err = missing.lookupResume(context.Background(), "claude", "unreachable-session")
	if err == nil || !rpc.IsConnectError(err) || found || !reflect.DeepEqual(got, resumeTarget{}) {
		t.Fatalf("connection failure target=%+v found=%v err=%v", got, found, err)
	}
}

func TestResolveSessionScopeBuildsRootsStableIDsAndAnnotations(t *testing.T) {
	home := isolatedResumeHome(t)
	root := filepath.Join(home, "workspace-root")
	label := filepath.Base(root)
	handler := &resumeTestRPCHandler{scope: daemon.WorkspaceScope{
		Root:      root,
		SlotPaths: []string{filepath.Join(root, "slot-current"), filepath.Join(root, "slot-retired")},
		Sessions: []state.SessionScope{
			{Agent: "claude", AgentSessionID: "active-native", State: "ACTIVE"},
			{Agent: "claude", AgentSessionID: "archived-native", State: "ARCHIVED"},
			{Agent: "codex", AgentSessionID: "expired-native", State: "ARCHIVED"},
			{Agent: "codex", AgentSessionID: "pending-native", State: "RELEASING"},
		},
	}}
	client := serveResumeTestRPC(t, handler)

	scope, err := client.ResolveSessionScope(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	wantRoots := []sessions.ScopeRoot{
		{Prefix: root, Label: label},
		{Prefix: filepath.Join(root, "slot-current"), Label: label},
		{Prefix: filepath.Join(root, "slot-retired"), Label: label},
	}
	if !reflect.DeepEqual(scope.Roots, wantRoots) {
		t.Fatalf("scope roots=%v, want %v", scope.Roots, wantRoots)
	}
	wantStableIDs := testSessionStableIDs("claude", []string{"active-native", "archived-native"})
	wantStableIDs = append(wantStableIDs, testSessionStableIDs("codex", []string{"expired-native", "pending-native"})...)
	if !reflect.DeepEqual(scope.StableIDs, wantStableIDs) {
		t.Fatalf("scope stable IDs=%v, want %v", scope.StableIDs, wantStableIDs)
	}
	wantAnnotations := map[string]struct {
		text  string
		inUse bool
	}{
		wantStableIDs[0]: {text: "使用中", inUse: true},
		wantStableIDs[1]: {},
		wantStableIDs[2]: {},
		wantStableIDs[3]: {},
	}
	if len(scope.Annotations) != len(wantAnnotations) {
		t.Fatalf("scope annotations=%v, want %v", scope.Annotations, wantAnnotations)
	}
	for stableID, want := range wantAnnotations {
		annotation, ok := scope.Annotations[stableID]
		if !ok || annotation.Text != want.text || annotation.InUse != want.inUse {
			t.Fatalf("annotation[%q]=%+v, want text=%q in_use=%v", stableID, annotation, want.text, want.inUse)
		}
	}

	params := handler.paramsFor("WorkspaceScope")
	var request map[string]string
	if err := json.Unmarshal(params, &request); err != nil {
		t.Fatal(err)
	}
	if request["cwd"] != root {
		t.Fatalf("WorkspaceScope cwd=%q, want %q", request["cwd"], root)
	}
}

func isolatedResumeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	return home
}

type resumeTestRPCHandler struct {
	mu           sync.Mutex
	methods      []string
	params       map[string]json.RawMessage
	scope        daemon.WorkspaceScope
	resumeStatus resumeStatus
	resumeErr    error
}

func (h *resumeTestRPCHandler) Handle(_ context.Context, method string, raw json.RawMessage) (any, error) {
	h.mu.Lock()
	h.methods = append(h.methods, method)
	if h.params == nil {
		h.params = map[string]json.RawMessage{}
	}
	h.params[method] = append(json.RawMessage(nil), raw...)
	h.mu.Unlock()

	switch method {
	case "Status":
		return map[string]any{"ok": true}, nil
	case "WorkspaceScope":
		return h.scope, nil
	case "ResumeStatus":
		if h.resumeErr != nil {
			return nil, h.resumeErr
		}
		return h.resumeStatus, nil
	default:
		return map[string]any{"ok": true}, nil
	}
}

func (h *resumeTestRPCHandler) calledMethods() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.methods...)
}

func (h *resumeTestRPCHandler) paramsFor(method string) json.RawMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append(json.RawMessage(nil), h.params[method]...)
}

func serveResumeTestRPC(t *testing.T, handler *resumeTestRPCHandler) Client {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "wxd.sock")
	ctx, cancel := context.WithCancel(context.Background())
	server := &rpc.Server{Socket: socket, Handler: handler}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	waitForPath(t, socket)
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("resume test RPC server: %v", err)
		}
	})
	return Client{RPC: rpc.Client{Socket: socket, Timeout: time.Second}, Config: config.Defaults()}
}

func TestResolveSessionScopeKeepsRestoringConversationInUse(t *testing.T) {
	home := isolatedResumeHome(t)
	parent := state.SessionScope{Agent: "claude", AgentSessionID: "shared", State: "ARCHIVED"}
	child := state.SessionScope{Agent: "claude", AgentSessionID: "shared", State: "RESTORING"}
	for _, rows := range [][]state.SessionScope{{child, parent}, {parent, child}} {
		client := serveResumeTestRPC(t, &resumeTestRPCHandler{scope: daemon.WorkspaceScope{Root: home, Sessions: rows}})
		scope, err := client.ResolveSessionScope(context.Background(), home)
		if err != nil {
			t.Fatal(err)
		}
		id := identity.ComputeSessionStableID("claude", "shared")
		if a := scope.Annotations[id]; !a.InUse || a.Text != "使用中" {
			t.Fatalf("annotation=%+v", a)
		}
	}
}

func testSessionStableIDs(tool string, nativeIDs []string) []string {
	ids := make([]string, 0, len(nativeIDs))
	for _, id := range nativeIDs {
		ids = append(ids, identity.ComputeSessionStableID(tool, id))
	}
	return ids
}
