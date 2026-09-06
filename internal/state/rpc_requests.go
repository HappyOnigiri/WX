package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

func (s *Store) BeginRPCRequest(ctx context.Context, key, method, params string, expiresAt time.Time) ([]byte, string, string, bool, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", "", false, err
	}
	defer tx.Rollback()
	paramsHash := rpcParamsHash(params)
	res, err := tx.ExecContext(ctx, `INSERT INTO rpc_idempotency(idempotency_key,method,params,result,error_code,error_message,completed_at,expires_at,state) VALUES(?,?,?,NULL,NULL,NULL,?,?,'PENDING') ON CONFLICT(idempotency_key) DO NOTHING`, key, method, paramsHash, now(), FormatTime(expiresAt))
	if err != nil {
		return nil, "", "", false, err
	}
	inserted, _ := res.RowsAffected()
	if inserted == 1 {
		return nil, "", "", true, tx.Commit()
	}
	var storedMethod, storedParams, requestState, errorCode, errorMessage string
	var result []byte
	if err := tx.QueryRowContext(ctx, `SELECT method,params,state,result,COALESCE(error_code,''),COALESCE(error_message,'') FROM rpc_idempotency WHERE idempotency_key=? AND expires_at>?`, key, now()).Scan(&storedMethod, &storedParams, &requestState, &result, &errorCode, &errorMessage); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "IDEMPOTENCY_EXPIRED", "idempotency reservation expired before it could be reused", false, nil
		}
		return nil, "", "", false, err
	}
	if storedMethod != method || storedParams != paramsHash {
		return nil, "IDEMPOTENCY_KEY_REUSE", "idempotency key was reused with a different method or payload", false, nil
	}
	if requestState == "PENDING" {
		return nil, "IDEMPOTENCY_INDETERMINATE", "a prior request crossed the durable mutation boundary without committing its response; wx will not execute it again", false, nil
	}
	if requestState != "COMPLETED" {
		return nil, "", "", false, fmt.Errorf("unknown idempotency reservation state %q", requestState)
	}
	return result, errorCode, errorMessage, false, nil
}

func (s *Store) CompleteRPCRequest(ctx context.Context, key, method, params string, result []byte, errorCode, errorMessage string, expiresAt time.Time) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	paramsHash := rpcParamsHash(params)
	res, err := s.db.ExecContext(ctx, `UPDATE rpc_idempotency SET result=?,error_code=?,error_message=?,completed_at=?,expires_at=?,state='COMPLETED' WHERE idempotency_key=? AND method=? AND params=? AND state='PENDING'`, result, nullString(errorCode), nullString(errorMessage), now(), FormatTime(expiresAt), key, method, paramsHash)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("idempotency reservation is not pending for this request")
	}
	return nil
}

func rpcParamsHash(params string) string {
	digest := sha256.Sum256([]byte(params))
	return hex.EncodeToString(digest[:])
}
