package agent

import (
	"encoding/json"
	"testing"
)

func FuzzHookPayload(f *testing.F) {
	for _, seed := range []string{
		`{"session_id":"session-1","source":"startup"}`,
		`{"session_id":"session-2","source":"resume","extra":{"nested":true}}`,
		`{}`,
		`null`,
		`{`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var payload HookInput
		err := json.Unmarshal(data, &payload)
		if err != nil {
			return
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		var roundTrip HookInput
		if err := json.Unmarshal(encoded, &roundTrip); err != nil {
			t.Fatal(err)
		}
		if roundTrip != payload {
			t.Fatalf("round trip=%+v, want %+v", roundTrip, payload)
		}
	})
}
