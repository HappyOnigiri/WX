package state

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecoverJobsReclaimsOnlyExpiredLease(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	job, err := store.CreateJob(ctx, "ENSURE_STANDBY", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimJob(ctx, job.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("live lease was reclaimed: %+v", jobs)
	}
	if _, err := store.db.Exec(`UPDATE jobs SET lease_expires_at=? WHERE id=?`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), job.ID); err != nil {
		t.Fatal(err)
	}
	jobs, err = store.RecoverJobs(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != job.ID || jobs[0].State != "PENDING" {
		t.Fatalf("expired lease recovery=%+v", jobs)
	}
}

func TestEnsureRecoveryJobsReconstructsInterruptedSlotAndRepositoryWork(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	for _, slot := range []struct {
		id, state, sessionState string
	}{
		{id: "prepare", state: "PREPARING", sessionState: "STARTING"},
		{id: "snapshot", state: "SNAPSHOTTING", sessionState: "SNAPSHOTTING"},
		{id: "remove", state: "REMOVING", sessionState: "ARCHIVED"},
	} {
		if _, err := store.db.Exec(`INSERT INTO slots(id,workspace_id,generation,root_id,rel_path,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, slot.id, "workspace", 1, testRootID, filepath.Join("workspace", slot.id), slot.state, now(), now()); err != nil {
			t.Fatal(err)
		}
		if slot.sessionState != "" {
			if _, err := store.db.Exec(`INSERT INTO sessions(id,workspace_id,slot_id,state,agent_kind,session_token_hash,created_at) VALUES(?,?,?,?,?,?,?)`, slot.id, "workspace", slot.id, slot.sessionState, "codex", HashToken(slot.id), now()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := store.db.Exec(`INSERT INTO slots(id,workspace_id,generation,root_id,rel_path,state,created_at,updated_at) VALUES('retire','workspace',1,?,'workspace/retire','RETIRING',?,?)`, testRootID, now(), now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO slot_repositories(slot_id,repository_id,dir_name,state,requested_ref,base_oid,prepare_fingerprint) VALUES('retire','repository','repository','RETIRING','main','abc','fp')`); err != nil {
		t.Fatal(err)
	}

	jobs, err := store.EnsureRecoveryJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"prepare": "PREPARE", "snapshot": "SNAPSHOT", "remove": "REMOVE", "retire": "REMOVE_REPOSITORY"}
	if len(jobs) != len(want) {
		t.Fatalf("recovery jobs=%+v", jobs)
	}
	for _, job := range jobs {
		if want[job.SlotID] != job.Kind {
			t.Fatalf("unexpected recovery job=%+v", job)
		}
		if job.Kind == "REMOVE" && job.SessionID != "remove" {
			t.Fatalf("remove recovery job lost archived session identity: %+v", job)
		}
		if job.Kind == "REMOVE_REPOSITORY" && job.RepositoryID != "repository" {
			t.Fatalf("repository recovery job lost repository identity: %+v", job)
		}
	}
	if duplicate, err := store.EnsureRecoveryJobs(ctx); err != nil || len(duplicate) != 0 {
		t.Fatalf("duplicate recovery jobs=%+v err=%v", duplicate, err)
	}
}

func TestJobEventsRecordAttemptsRetriesAndElapsedTime(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	job, err := store.CreateJob(ctx, "PREPARE", "", "slot", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimJob(ctx, job.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := store.RetryJob(ctx, job.ID, "owner", time.Millisecond, "TRANSIENT"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE jobs SET not_before=? WHERE id=?`, now(), job.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimJob(ctx, job.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Attempt != 2 {
		t.Fatalf("attempt=%d", claimed.Attempt)
	}
	if err := store.FinishJob(ctx, job.ID, "owner", nil); err != nil {
		t.Fatal(err)
	}
	rows, err := store.db.Query(`SELECT kind,message FROM events WHERE slot_id='slot' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events []string
	for rows.Next() {
		var kind, message string
		if err := rows.Scan(&kind, &message); err != nil {
			t.Fatal(err)
		}
		events = append(events, kind+" "+message)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(events, "\n")
	for _, fragment := range []string{"job_started kind=PREPARE attempt=1", "job_retry delay=1ms failure_code=TRANSIENT", "job_started kind=PREPARE attempt=2", "PREPARE state=SUCCEEDED elapsed="} {
		if !strings.Contains(joined, fragment) {
			t.Fatalf("event log missing %q:\n%s", fragment, joined)
		}
	}
}

func TestNewJobPersistenceRollsBackOnLateDatabaseFaults(t *testing.T) {
	for _, test := range []struct {
		name    string
		trigger string
		retry   bool
	}{
		{name: "finish update", trigger: `CREATE TRIGGER fail_finish_update BEFORE UPDATE OF state ON jobs WHEN NEW.state='SUCCEEDED' BEGIN SELECT RAISE(ABORT,'fault'); END`},
		{name: "finish ignored update", trigger: `CREATE TRIGGER ignore_finish_update BEFORE UPDATE OF state ON jobs WHEN NEW.state='SUCCEEDED' BEGIN SELECT RAISE(IGNORE); END`},
		{name: "finish event", trigger: `CREATE TRIGGER fail_finish_event BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT,'fault'); END`},
		{name: "retry event", trigger: `CREATE TRIGGER fail_retry_event BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT,'fault'); END`, retry: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			ctx := context.Background()
			job, err := store.CreateJob(ctx, "PREPARE", "", "slot", "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.ClaimJob(ctx, job.ID, "owner"); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec(test.trigger); err != nil {
				t.Fatal(err)
			}
			if test.retry {
				err = store.RetryJob(ctx, job.ID, "owner", time.Second, "TRANSIENT")
			} else {
				err = store.FinishJob(ctx, job.ID, "owner", nil)
			}
			if err == nil {
				t.Fatal("fault-injected job transition succeeded")
			}
			var stateName string
			if err := store.db.QueryRow(`SELECT state FROM jobs WHERE id=?`, job.ID).Scan(&stateName); err != nil || stateName != "RUNNING" {
				t.Fatalf("job transition was not rolled back: state=%s err=%v", stateName, err)
			}
		})
	}
	t.Run("invalid start time", func(t *testing.T) {
		store := openTestStore(t)
		ctx := context.Background()
		job, err := store.CreateJob(ctx, "PREPARE", "", "slot", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.ClaimJob(ctx, job.ID, "owner"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`UPDATE jobs SET started_at='invalid' WHERE id=?`, job.ID); err != nil {
			t.Fatal(err)
		}
		if err := store.FinishJob(ctx, job.ID, "owner", nil); err != nil {
			t.Fatal(err)
		}
	})
}

func TestEnsureRecoveryJobsReportsQueryAndInsertFaults(t *testing.T) {
	t.Run("query", func(t *testing.T) {
		store := openTestStore(t)
		if _, err := store.db.Exec(`DROP TABLE slots`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.EnsureRecoveryJobs(context.Background()); err == nil {
			t.Fatal("recovery reconstruction succeeded without slots")
		}
	})
	t.Run("insert", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		if _, err := store.db.Exec(`INSERT INTO slots(id,workspace_id,generation,root_id,rel_path,state,created_at,updated_at) VALUES('slot','workspace',1,?,'workspace/slot','PREPARING',?,?)`, testRootID, now(), now()); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_recovery_job BEFORE INSERT ON jobs BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.EnsureRecoveryJobs(context.Background()); err == nil {
			t.Fatal("recovery reconstruction succeeded despite job insert fault")
		}
	})
}

func TestJobClaimRollsBackWhenClaimedRowOrAuditEventChanges(t *testing.T) {
	for _, test := range []struct {
		name    string
		trigger string
	}{
		{
			name:    "claimed row disappears",
			trigger: `CREATE TRIGGER remove_claimed_job AFTER UPDATE OF state ON jobs WHEN NEW.state='RUNNING' BEGIN DELETE FROM jobs WHERE id=NEW.id; END`,
		},
		{
			name:    "audit event fails",
			trigger: `CREATE TRIGGER fail_claim_event BEFORE INSERT ON events WHEN NEW.kind='job_started' BEGIN SELECT RAISE(ABORT,'event fault'); END`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			job, err := store.CreateJob(context.Background(), "PREPARE", "", "", "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec(test.trigger); err != nil {
				t.Fatal(err)
			}
			if _, err := store.ClaimJob(context.Background(), job.ID, "worker"); err == nil {
				t.Fatal("fault-injected job claim succeeded")
			}
			var state string
			if err := store.db.QueryRow(`SELECT state FROM jobs WHERE id=?`, job.ID).Scan(&state); err != nil || state != "PENDING" {
				t.Fatalf("failed claim did not roll back job: state=%q err=%v", state, err)
			}
		})
	}
}

func TestEnsureRecoveryJobsPropagatesJobInsertionFault(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	if _, err := store.db.Exec(`INSERT INTO slots(id,workspace_id,generation,root_id,rel_path,state,created_at,updated_at) VALUES('recovery-fault','workspace',1,?,'workspace/recovery-fault','PREPARING',?,?)`, testRootID, now(), now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER fail_recovery_job BEFORE INSERT ON jobs WHEN NEW.kind='PREPARE' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureRecoveryJobs(ctx); err == nil {
		t.Fatal("recovery job reconstruction succeeded despite a job insertion fault")
	}
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM jobs`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failed recovery reconstruction left partial jobs: %d", count)
	}
}

func TestDeferJobPropagatesUpdateFault(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	job, err := store.CreateJob(ctx, "RESTORE", "workspace", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimJob(ctx, job.ID, "worker"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_defer_job BEFORE UPDATE OF state ON jobs WHEN OLD.id='%s' AND NEW.state='PENDING' BEGIN SELECT RAISE(ABORT,'fault'); END`, job.ID)); err != nil {
		t.Fatal(err)
	}
	if err := store.DeferJob(ctx, job.ID, "worker", 0, "SNAPSHOT_PENDING"); err == nil {
		t.Fatal("job deferral succeeded despite an update fault")
	}
}

func TestRecoverJobsPropagatesReclaimUpdateFault(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	job, err := store.CreateJob(ctx, "ENSURE_STANDBY", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimJob(ctx, job.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE jobs SET lease_expires_at=? WHERE id=?`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER fail_recover_jobs BEFORE UPDATE OF state ON jobs WHEN NEW.state='PENDING' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecoverJobs(ctx, false); err == nil {
		t.Fatal("job recovery succeeded despite a reclaim update fault")
	}
}
