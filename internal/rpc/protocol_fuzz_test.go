package rpc

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"testing"
)

func FuzzReadFrame(f *testing.F) {
	framed := func(payload string) []byte {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(payload)))
		return append(size[:], payload...)
	}
	f.Add(framed(`{"version":1,"id":"a","method":"WaitReady"}`))
	f.Add(framed(`{`))
	f.Add(framed(``))
	f.Add([]byte{0, 0, 0})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Add(append([]byte{0x00, 0x90, 0x00, 0x00}, []byte(`{"version":1}`)...))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			t.Skip()
		}
		var request Request
		err := readFrame(bytes.NewReader(data), &request)
		if len(data) < 4 {
			if err == nil {
				t.Fatalf("short input %q accepted", data)
			}
			return
		}
		// 宣言長は割り当て前に検査される。範囲外の長さを受け入れると maxFrame を超える確保を招く。
		declared := binary.BigEndian.Uint32(data[:4])
		if (declared == 0 || declared > maxFrame) && err == nil {
			t.Fatalf("frame length %d accepted", declared)
		}
		if uint64(declared) > uint64(len(data)-4) && err == nil {
			t.Fatalf("frame declaring %d bytes accepted with %d available", declared, len(data)-4)
		}
		if err != nil {
			return
		}
		var buffer bytes.Buffer
		if err := writeFrame(&buffer, request); err != nil {
			t.Fatal(err)
		}
		var roundTrip Request
		if err := readFrame(&buffer, &roundTrip); err != nil {
			t.Fatalf("re-read of written frame: %v", err)
		}
		if !equalRequests(roundTrip, request) {
			t.Fatalf("round trip=%+v, want %+v", roundTrip, request)
		}
	})
}

func equalRequests(a, b Request) bool {
	return a.Version == b.Version && a.ID == b.ID && a.Method == b.Method &&
		a.Deadline == b.Deadline && a.IdempotencyKey == b.IdempotencyKey &&
		bytes.Equal(canonicalJSON(a.Params), canonicalJSON(b.Params))
}

// canonicalJSON は Params の表記揺れ（空白・キー順・数値表現）を除いて比較するために再エンコードする。
func canonicalJSON(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return raw
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return raw
	}
	return encoded
}
