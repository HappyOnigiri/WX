package archive

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/state"
)

func FuzzSnapshotMetadata(f *testing.F) {
	valid, err := json.Marshal(state.Snapshot{
		ID: "0123456789abcdef0123456789abcdef", SessionID: "session", RepositoryID: "repository",
		HeadOID: "0123456789012345678901234567890123456789", HeadRef: "refs/wx/recovery/session/repository/head",
		IndexTreeOID: "0123456789012345678901234567890123456789", WorktreeOID: "0123456789012345678901234567890123456789",
		WorktreeRef: "refs/wx/recovery/session/repository/worktree", Status: "ARCHIVED",
		CreatedAt: state.FormatTime(time.Unix(0, 0)), ExpiresAt: state.FormatTime(time.Unix(3600, 0)),
	})
	if err != nil {
		f.Fatal(err)
	}
	for _, seed := range [][]byte{valid, []byte(`{}`), []byte(`null`), []byte(`{"expires_at":"invalid"}`), []byte(`{`)} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var snapshot state.Snapshot
		if err := json.Unmarshal(data, &snapshot); err != nil {
			return
		}
		encoded, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		var roundTrip state.Snapshot
		if err := json.Unmarshal(encoded, &roundTrip); err != nil {
			t.Fatal(err)
		}
		if roundTrip != snapshot {
			t.Fatalf("round trip=%+v, want %+v", roundTrip, snapshot)
		}
		if snapshot.ExpiresAt != "" {
			_, _ = time.Parse(time.RFC3339Nano, snapshot.ExpiresAt)
		}
	})
}
