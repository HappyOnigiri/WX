package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func FuzzHookPayload(f *testing.F) {
	for _, seed := range []string{
		`{"session_id":"abc","source":"resume","cwd":"/tmp/wx"}`,
		`{"session_id":"abc"}`,
		`   `,
		``,
		`null`,
		`{`,
		`[1,2,3]`,
		`{"session_id":123}`,
		`{"cwd":"/tmp/wx\nfake"}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		// 1MiB 超は decodeHookPayload が切り詰めるため round trip の対象にならず、実行速度だけを落とす。
		if len(data) > 1<<20 {
			t.Skip()
		}
		payload, err := decodeHookPayload(strings.NewReader(string(data)))
		if err != nil {
			if payload != (HookInput{}) {
				t.Fatalf("decode failed but returned payload %+v", payload)
			}
			return
		}
		if len(strings.TrimSpace(string(data))) == 0 && payload != (HookInput{}) {
			t.Fatalf("blank input produced payload %+v", payload)
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		roundTrip, err := decodeHookPayload(strings.NewReader(string(encoded)))
		if err != nil {
			t.Fatalf("re-decode of %q: %v", encoded, err)
		}
		if roundTrip != payload {
			t.Fatalf("round trip=%+v, want %+v", roundTrip, payload)
		}
	})
}
