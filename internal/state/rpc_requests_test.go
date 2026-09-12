package state

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRPCIdempotencyResultSurvivesStoreRestart(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	params := `{"value":1}`
	resultPayload := []byte(`{"value":"response"}`)
	if _, _, _, execute, err := store.BeginRPCRequest(ctx, "key", "Mutate", params, time.Now().Add(time.Hour)); err != nil || !execute {
		t.Fatalf("begin execute=%v err=%v", execute, err)
	}
	if err := store.CompleteRPCRequest(ctx, "key", "Mutate", params, resultPayload, "", "", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var storedResult []byte
	if err := store.db.QueryRow(`SELECT result FROM rpc_idempotency WHERE idempotency_key='key'`).Scan(&storedResult); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(storedResult, resultPayload) {
		t.Fatalf("stored RPC result=%q want=%q", storedResult, resultPayload)
	}
	if _, _, _, execute, err := store.BeginRPCRequest(ctx, "pending", "Mutate", `{}`, time.Now().Add(time.Hour)); err != nil || !execute {
		t.Fatalf("pending begin execute=%v err=%v", execute, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	result, code, message, execute, err := store.BeginRPCRequest(ctx, "key", "Mutate", params, time.Now().Add(time.Hour))
	if err != nil || execute || string(result) != string(resultPayload) || code != "" || message != "" {
		t.Fatalf("result=%s code=%q message=%q execute=%v err=%v", result, code, message, execute, err)
	}
	_, code, _, execute, err = store.BeginRPCRequest(ctx, "key", "Mutate", `{"value":2}`, time.Now().Add(time.Hour))
	if err != nil || execute || code != "IDEMPOTENCY_KEY_REUSE" {
		t.Fatalf("mismatch code=%q execute=%v err=%v", code, execute, err)
	}
	_, code, _, execute, err = store.BeginRPCRequest(ctx, "pending", "Mutate", `{}`, time.Now().Add(time.Hour))
	if err != nil || execute || code != "IDEMPOTENCY_INDETERMINATE" {
		t.Fatalf("pending code=%q execute=%v err=%v", code, execute, err)
	}
	if _, _, _, execute, err := store.BeginRPCRequest(ctx, "expired", "Mutate", `{}`, time.Now().Add(-time.Hour)); err != nil || !execute {
		t.Fatalf("expired begin execute=%v err=%v", execute, err)
	}
	if err := store.PruneMetadata(ctx, FormatTime(time.Now().Add(-time.Hour)), FormatTime(time.Now().Add(-time.Hour)), FormatTime(time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	var expired int
	if err := store.db.QueryRow(`SELECT count(*) FROM rpc_idempotency WHERE idempotency_key='expired'`).Scan(&expired); err != nil || expired != 0 {
		t.Fatalf("expired idempotency rows=%d err=%v", expired, err)
	}
}

func TestRPCIdempotencyRejectsInvalidReservationTransitions(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	expiry := time.Now().Add(time.Hour)

	if err := store.CompleteRPCRequest(ctx, "missing", "Mutate", `{}`, nil, "", "", expiry); err == nil {
		t.Fatal("completion without a reservation succeeded")
	}
	if _, _, _, execute, err := store.BeginRPCRequest(ctx, "complete", "Mutate", `{}`, expiry); err != nil || !execute {
		t.Fatalf("begin execute=%v err=%v", execute, err)
	}
	if err := store.CompleteRPCRequest(ctx, "complete", "Mutate", `{}`, nil, "EXPECTED", "details", expiry); err != nil {
		t.Fatal(err)
	}
	result, code, message, execute, err := store.BeginRPCRequest(ctx, "complete", "Mutate", `{}`, expiry)
	if err != nil || execute || result != nil || code != "EXPECTED" || message != "details" {
		t.Fatalf("replay result=%v code=%q message=%q execute=%v err=%v", result, code, message, execute, err)
	}
	if err := store.CompleteRPCRequest(ctx, "complete", "Mutate", `{}`, nil, "", "", expiry); err == nil {
		t.Fatal("completed reservation was completed twice")
	}
	if err := store.CompleteRPCRequest(ctx, "complete", "Other", `{}`, nil, "", "", expiry); err == nil {
		t.Fatal("reservation completed with a different method")
	}

	key := "invalid-state"
	if _, _, _, execute, err := store.BeginRPCRequest(ctx, key, "Mutate", `{}`, expiry); err != nil || !execute {
		t.Fatalf("begin execute=%v err=%v", execute, err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE rpc_idempotency SET state=? WHERE idempotency_key=?`, "UNKNOWN", key); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := store.BeginRPCRequest(ctx, key, "Mutate", `{}`, expiry); err == nil || !strings.Contains(err.Error(), "unknown idempotency reservation state") {
		t.Fatalf("error=%v, want unknown reservation state", err)
	}

	if _, _, _, execute, err := store.BeginRPCRequest(ctx, "expired", "Mutate", `{}`, time.Now().Add(-time.Minute)); err != nil || !execute {
		t.Fatalf("begin expired execute=%v err=%v", execute, err)
	}
	_, code, _, execute, err = store.BeginRPCRequest(ctx, "expired", "Mutate", `{}`, expiry)
	if err != nil || execute || code != "IDEMPOTENCY_EXPIRED" {
		t.Fatalf("expired replay code=%q execute=%v err=%v", code, execute, err)
	}
}

func TestRPCIdempotencyPropagatesReservationAndCompletionStorageFaults(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	expiry := time.Now().Add(time.Hour)
	if _, err := store.db.Exec(`CREATE TRIGGER fail_rpc_reservation BEFORE INSERT ON rpc_idempotency BEGIN SELECT RAISE(ABORT,'reservation fault'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := store.BeginRPCRequest(ctx, "reservation", "Mutate", `{}`, expiry); err == nil {
		t.Fatal("RPC reservation storage fault was ignored")
	}
	if _, err := store.db.Exec(`DROP TRIGGER fail_rpc_reservation`); err != nil {
		t.Fatal(err)
	}
	if _, _, _, execute, err := store.BeginRPCRequest(ctx, "completion", "Mutate", `{}`, expiry); err != nil || !execute {
		t.Fatalf("begin completion execute=%v err=%v", execute, err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER fail_rpc_completion BEFORE UPDATE ON rpc_idempotency BEGIN SELECT RAISE(ABORT,'completion fault'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteRPCRequest(ctx, "completion", "Mutate", `{}`, nil, "", "", expiry); err == nil {
		t.Fatal("RPC completion storage fault was ignored")
	}
	if _, err := store.db.Exec(`DROP TRIGGER fail_rpc_completion`); err != nil {
		t.Fatal(err)
	}

	paramsHash := rpcParamsHash(`{}`)
	if _, err := store.db.Exec(`INSERT INTO rpc_idempotency(idempotency_key,method,params,result,error_code,error_message,completed_at,expires_at,state) VALUES('unknown','Mutate',?,NULL,NULL,NULL,?,?, 'UNKNOWN')`, paramsHash, now(), FormatTime(expiry)); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := store.BeginRPCRequest(ctx, "unknown", "Mutate", `{}`, expiry); err == nil || !strings.Contains(err.Error(), "unknown idempotency reservation state") {
		t.Fatalf("unknown reservation state error=%v", err)
	}

	blockingBackupDirectory := store.path + ".backups"
	if err := os.WriteFile(blockingBackupDirectory, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Backup(ctx, 1, time.Hour); err == nil {
		t.Fatal("backup succeeded through a regular-file backup directory")
	}
}
