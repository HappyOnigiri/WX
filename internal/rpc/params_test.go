package rpc

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// 共有型の round-trip だけでは CLI と daemon が同時に誤るため、送信する固定 JSON と byte 単位で比較する。
// 冪等キーと再送判定は Params の JSON 文字列を比較するので、キーの順序・有無・型が変わると同じ要求が別物になる。
// 貸出の 3 フィールドと準備設定の上書き 2 フィールドは辞書順の位置に入り、指定がなくても空文字・null として必ず出力される。

func TestResolveAndLeaseParamsMarshalsLegacyBytes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		params ResolveAndLeaseParams
		want   string
	}{
		{
			name:   "zero",
			params: ResolveAndLeaseParams{},
			want:   `{"agent":"","branches":null,"client_pid":0,"cwd":"","force_worktree":false,"lease_kind":"","lease_owner_session_id":"","lease_owner_token":"","prepare_copy_mode":"","prepare_cow_min_size_kib":null}`,
		},
		{
			name:   "populated",
			params: ResolveAndLeaseParams{Agent: "codex", Branches: []string{"main", "topic"}, ClientPID: 4321, CWD: "/repo", ForceWorktree: true},
			want:   `{"agent":"codex","branches":["main","topic"],"client_pid":4321,"cwd":"/repo","force_worktree":true,"lease_kind":"","lease_owner_session_id":"","lease_owner_token":"","prepare_copy_mode":"","prepare_cow_min_size_kib":null}`,
		},
		{
			name:   "empty branches",
			params: ResolveAndLeaseParams{Branches: []string{}},
			want:   `{"agent":"","branches":[],"client_pid":0,"cwd":"","force_worktree":false,"lease_kind":"","lease_owner_session_id":"","lease_owner_token":"","prepare_copy_mode":"","prepare_cow_min_size_kib":null}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			encoded, err := json.Marshal(tc.params)
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != tc.want {
				t.Fatalf("ResolveAndLeaseParams bytes=%s, want %s", encoded, tc.want)
			}
		})
	}
}

func TestResumeParamsMarshalsLegacyBytes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		params ResumeParams
		want   string
	}{
		{
			name:   "zero",
			params: ResumeParams{},
			want:   `{"agent":"","agent_session_id":"","branches":null,"client_pid":0,"fresh":false,"lease_kind":"","lease_owner_session_id":"","lease_owner_token":"","wx_session_id":""}`,
		},
		{
			name:   "populated",
			params: ResumeParams{Agent: "claude", AgentSessionID: "native", Branches: []string{"main"}, ClientPID: 99, Fresh: true, WXSessionID: "wx-1"},
			want:   `{"agent":"claude","agent_session_id":"native","branches":["main"],"client_pid":99,"fresh":true,"lease_kind":"","lease_owner_session_id":"","lease_owner_token":"","wx_session_id":"wx-1"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			encoded, err := json.Marshal(tc.params)
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != tc.want {
				t.Fatalf("ResumeParams bytes=%s, want %s", encoded, tc.want)
			}
		})
	}
}

// map[string]any の marshal はキーを辞書順に並べるため、同じ値なら byte 列が共有型と一致する。
// 貸出コマンドが載せる 3 フィールドも同じ並びに入ることをここで固定する。
func TestSharedParamsMatchLegacyMapEncoding(t *testing.T) {
	t.Parallel()
	pid := os.Getpid()
	lease, err := json.Marshal(map[string]any{"cwd": "/repo", "branches": []string{"main"}, "agent": "codex", "client_pid": pid, "force_worktree": true, "lease_kind": "shell", "lease_owner_session_id": "owner", "lease_owner_token": "token", "prepare_copy_mode": "", "prepare_cow_min_size_kib": nil})
	if err != nil {
		t.Fatal(err)
	}
	shared, err := json.Marshal(ResolveAndLeaseParams{Agent: "codex", Branches: []string{"main"}, ClientPID: pid, CWD: "/repo", ForceWorktree: true, LeaseKind: "shell", LeaseOwnerSessionID: "owner", LeaseOwnerToken: "token"})
	if err != nil {
		t.Fatal(err)
	}
	if string(shared) != string(lease) {
		t.Fatalf("ResolveAndLeaseParams=%s, legacy map=%s", shared, lease)
	}
	legacyResume, err := json.Marshal(map[string]any{"wx_session_id": "wx-1", "agent": "claude", "client_pid": pid, "agent_session_id": "native", "fresh": false, "branches": []string(nil), "lease_kind": "", "lease_owner_session_id": "", "lease_owner_token": ""})
	if err != nil {
		t.Fatal(err)
	}
	sharedResume, err := json.Marshal(ResumeParams{Agent: "claude", AgentSessionID: "native", ClientPID: pid, WXSessionID: "wx-1"})
	if err != nil {
		t.Fatal(err)
	}
	if string(sharedResume) != string(legacyResume) {
		t.Fatalf("ResumeParams=%s, legacy map=%s", sharedResume, legacyResume)
	}
}

// 欠落・null・空 object は既存 handler と同じくゼロ値へ落とす。順序の異なる旧 payload も同じ値を復元する。
func TestSharedParamsDecodeLegacyPayloads(t *testing.T) {
	t.Parallel()
	var lease ResolveAndLeaseParams
	if err := json.Unmarshal([]byte(`{"force_worktree":true,"cwd":"/repo","branches":null,"agent":"codex","client_pid":7}`), &lease); err != nil {
		t.Fatal(err)
	}
	want := ResolveAndLeaseParams{Agent: "codex", ClientPID: 7, CWD: "/repo", ForceWorktree: true}
	if !reflect.DeepEqual(lease, want) {
		t.Fatalf("decoded ResolveAndLeaseParams=%+v, want %+v", lease, want)
	}
	var resume ResumeParams
	if err := json.Unmarshal([]byte(`{"wx_session_id":"wx-1","fresh":true}`), &resume); err != nil {
		t.Fatal(err)
	}
	if resume.WXSessionID != "wx-1" || !resume.Fresh || resume.Agent != "" || resume.AgentSessionID != "" || resume.ClientPID != 0 || resume.Branches != nil {
		t.Fatalf("decoded ResumeParams=%+v, want only wx_session_id and fresh set", resume)
	}
	for _, raw := range []string{`{}`, `null`} {
		var empty ResumeParams
		if err := json.Unmarshal([]byte(raw), &empty); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		if !reflect.DeepEqual(empty, ResumeParams{}) {
			t.Fatalf("decoded %s=%+v, want zero value", raw, empty)
		}
	}
}
