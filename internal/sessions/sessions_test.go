package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/sessions/config"
	"github.com/HappyOnigiri/WX/internal/sessions/identity"
	"github.com/HappyOnigiri/WX/internal/sessions/scanner"
)

func TestScopeMatchesRootBoundariesAndArchivedIdentities(t *testing.T) {
	scope := &PickerScope{Scope: Scope{Roots: []ScopeRoot{{Prefix: "/work/project"}}, StableIDs: []string{"archived"}}}
	for _, tc := range []struct {
		cwd, id string
		want    bool
	}{
		{"/work/project", "", true}, {"/work/project/app", "", true}, {"/work/project-other", "", false}, {"/old/removed", "archived", true}, {"", "", false},
	} {
		if got := matchesScope(scanner.Session{CWD: tc.cwd, StableID: tc.id}, scope); got != tc.want {
			t.Fatalf("%+v: %v", tc, got)
		}
	}
	if !matchesScope(scanner.Session{}, nil) {
		t.Fatal("unrestricted scope rejected session")
	}
}

func writeHistory(t *testing.T, root, id, cwd string, at time.Time) {
	t.Helper()
	row, err := json.Marshal(map[string]any{"type": "user", "sessionId": id, "cwd": cwd, "message": map[string]any{"role": "user", "content": "タイトル"}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, id+".jsonl")
	if err := os.WriteFile(path, append(row, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func TestContinueReadsEachInvocationAndExcludesInUse(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	cfg := config.Config{Paths: config.PathsConfig{Claude: config.ToolPathsConfig{Sessions: []string{root}}}}
	scope := &PickerScope{Scope: Scope{Roots: []ScopeRoot{{Prefix: "/workspace"}}}, Annotations: map[string]Annotation{}}
	oldID := "11111111-1111-4111-8111-111111111111"
	newID := "22222222-2222-4222-8222-222222222222"
	outsideID := "33333333-3333-4333-8333-333333333333"
	writeHistory(t, root, oldID, "/workspace/repo", time.Unix(10, 0))
	opts := ContinueOptions{Tool: "claude", Scope: scope}
	target, found, err := Continue(context.Background(), cfg, opts)
	if err != nil || !found || target.SessionID != oldID {
		t.Fatalf("initial: %+v %v %v", target, found, err)
	}
	writeHistory(t, root, newID, "/workspace/repo", time.Unix(20, 0))
	writeHistory(t, root, outsideID, "/workspace-other", time.Unix(30, 0))
	target, found, err = Continue(context.Background(), cfg, opts)
	if err != nil || !found || target.SessionID != newID {
		t.Fatalf("new history: %+v %v %v", target, found, err)
	}
	scope.Annotations[identity.ComputeSessionStableID("claude", newID)] = Annotation{InUse: true}
	target, found, err = Continue(context.Background(), cfg, opts)
	if err != nil || !found || target.SessionID != oldID {
		t.Fatalf("in-use excluded: %+v %v %v", target, found, err)
	}
	target, found, err = Lookup(context.Background(), cfg, "claude", outsideID)
	if err != nil || !found || target.CWD != "/workspace-other" {
		t.Fatalf("cross-workspace lookup: %+v %v %v", target, found, err)
	}
	if _, found, err := Lookup(context.Background(), cfg, "claude", "missing"); err != nil || found {
		t.Fatalf("missing: %v %v", found, err)
	}
	scope.Annotations[identity.ComputeSessionStableID("claude", oldID)] = Annotation{InUse: true}
	if _, found, err := Continue(context.Background(), cfg, opts); err != nil || found {
		t.Fatalf("all in-use: %v %v", found, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := Continue(ctx, cfg, opts); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if _, _, err := Continue(context.Background(), cfg, ContinueOptions{Tool: "cursor"}); err == nil {
		t.Fatal("unsupported agent accepted")
	}
}
