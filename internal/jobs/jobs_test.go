package jobs_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"repolens/internal/jobs"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "jobs_test.db")
	db, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		t.Fatalf("failed opening sqlite: %v", err)
	}

	createTableQuery := `
	CREATE TABLE analysis_jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		job_type TEXT NOT NULL,
		resource_id TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'PENDING',
		execution_generation INTEGER NOT NULL DEFAULT 1,
		terminal_reason TEXT,
		attempt_count INTEGER NOT NULL DEFAULT 0,
		max_attempts INTEGER NOT NULL DEFAULT 3,
		next_run_at DATETIME NOT NULL,
		worker_id TEXT,
		claim_token TEXT,
		lease_until DATETIME,
		cancel_requested BOOLEAN NOT NULL DEFAULT 0,
		last_error_class TEXT,
		last_error_code TEXT,
		last_error_message TEXT,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL,
		finished_at DATETIME,
		UNIQUE(job_type, resource_id)
	);
	CREATE INDEX ix_job_claim ON analysis_jobs(status, next_run_at, created_at, id);
	CREATE INDEX ix_job_lease ON analysis_jobs(status, lease_until);
	`
	if _, err := db.Exec(createTableQuery); err != nil {
		t.Fatalf("failed creating test table: %v", err)
	}

	return db
}

func TestDiagnosisRetryPolicyUsesExplicitProviderErrorAllowlist(t *testing.T) {
	for code, want := range map[string]bool{
		"PROVIDER_TIMEOUT": true, "PROVIDER_CONNECTION_FAILED": true,
		"PROVIDER_UPSTREAM_ERROR": true, "PROVIDER_RATE_LIMITED": true,
		"HTTP_5XX_SERVER_ERROR": true, "TRANSIENT_NETWORK_ERROR": true,
		"INVALID_STRUCTURED_REPORT": false, "ATOMIC_FINALIZE_FAILED": false,
		"UNKNOWN_RETRYABLE_ERROR": false, "PROVIDER_AUTH_FAILED": false,
	} {
		if got := jobs.IsRetryableDiagnosisProviderError(code); got != want {
			t.Errorf("retry policy for %s = %t, want %t", code, got, want)
		}
	}
	if jobs.IsRetryableDiagnosisProviderFailure(jobs.ErrorClassPermanent, "PROVIDER_TIMEOUT") {
		t.Fatal("permanent provider error must not be retryable")
	}
	if !jobs.IsRetryableDiagnosisProviderFailure(jobs.ErrorClassPermanent, "PROVIDER_PROGRESS_ABORTED") {
		t.Fatal("provider failure after Agent progress must permit only explicit retry")
	}
	if !jobs.IsRetryableDiagnosisProviderFailure(jobs.ErrorClassRetryable, "PROVIDER_TIMEOUT") {
		t.Fatal("retryable provider timeout should be allowed")
	}
}

func TestErrorClassification(t *testing.T) {
	tests := []struct {
		err           error
		expectedClass jobs.ErrorClass
	}{
		{errors.New("connection reset by peer"), jobs.ErrorClassRetryable},
		{errors.New("dial tcp 127.0.0.1:80: connect: connection refused"), jobs.ErrorClassRetryable},
		{errors.New("context deadline exceeded"), jobs.ErrorClassRetryable},
		{errors.New("invalid url format"), jobs.ErrorClassPermanent},
		{errors.New("ssrf blocked link-local"), jobs.ErrorClassPermanent},
		{context.Canceled, jobs.ErrorClassCancelled},
		{jobs.ErrOwnershipLost, jobs.ErrorClassOwnershipLost},
		{jobs.NewPermanentError("BAD_REQ", "malformed json", nil), jobs.ErrorClassPermanent},
		{jobs.NewRetryableError("RATE_LIMIT", "rate limit exceeded", nil), jobs.ErrorClassRetryable},
	}

	for _, tt := range tests {
		class, _ := jobs.ClassifyError(tt.err)
		if class != tt.expectedClass {
			t.Errorf("ClassifyError(%v) = %v, want %v", tt.err, class, tt.expectedClass)
		}
	}
}

func TestHTTPStatusClassification(t *testing.T) {
	c429, _ := jobs.HTTPStatusToErrorClass(http.StatusTooManyRequests)
	if c429 != jobs.ErrorClassRetryable {
		t.Errorf("429 should be retryable")
	}

	c500, _ := jobs.HTTPStatusToErrorClass(http.StatusInternalServerError)
	if c500 != jobs.ErrorClassRetryable {
		t.Errorf("500 should be retryable")
	}

	c400, _ := jobs.HTTPStatusToErrorClass(http.StatusBadRequest)
	if c400 != jobs.ErrorClassPermanent {
		t.Errorf("400 should be permanent")
	}
}

func TestCalculateBackoff(t *testing.T) {
	b1 := jobs.CalculateBackoff(1, time.Second, time.Minute)
	if b1 < time.Second || b1 > 2*time.Second {
		t.Errorf("attempt 1 backoff unexpected: %v", b1)
	}

	b3 := jobs.CalculateBackoff(3, time.Second, time.Minute)
	if b3 < 4*time.Second || b3 > 6*time.Second {
		t.Errorf("attempt 3 backoff unexpected: %v", b3)
	}

	bMax := jobs.CalculateBackoff(10, time.Second, 10*time.Second)
	if bMax > 10*time.Second {
		t.Errorf("backoff exceeded max: %v", bMax)
	}
}

func TestStore_CreateAndClaim(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	store := jobs.NewStoreWithDriver(db, "sqlite3")
	ctx := context.Background()

	job := &jobs.AnalysisJob{
		JobType:    jobs.JobTypeRunDiagnosis,
		ResourceID: "diag-100",
	}

	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}
	if job.ID == 0 {
		t.Fatalf("expected job ID to be set")
	}

	// Claim with worker-1
	claimed, err := store.ClaimJobs(ctx, "worker-1", 5, 10*time.Second)
	if err != nil {
		t.Fatalf("ClaimJobs failed: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("expected 1 claimed job, got %d", len(claimed))
	}
	if *claimed[0].WorkerID != "worker-1" || claimed[0].ClaimToken == nil {
		t.Fatalf("claimed job missing worker ID or claim token")
	}

	// Second claim attempt should find nothing available
	claimed2, err := store.ClaimJobs(ctx, "worker-2", 5, 10*time.Second)
	if err != nil {
		t.Fatalf("ClaimJobs second attempt failed: %v", err)
	}
	if len(claimed2) != 0 {
		t.Fatalf("expected 0 claimed jobs on second attempt, got %d", len(claimed2))
	}
}

func TestStore_LeaseRenewalAndStaleFinalize(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	store := jobs.NewStoreWithDriver(db, "sqlite3")
	ctx := context.Background()

	job := &jobs.AnalysisJob{
		JobType:    jobs.JobTypeMaterializeSnapshot,
		ResourceID: "snap-1",
	}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}

	claimed, err := store.ClaimJobs(ctx, "worker-A", 1, 5*time.Second)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("failed claiming job: %v", err)
	}
	cj := claimed[0]

	// 1. Successful lease renewal
	newLease := time.Now().UTC().Add(30 * time.Second)
	if err := store.RenewLease(ctx, cj.ID, "worker-A", *cj.ClaimToken, newLease); err != nil {
		t.Fatalf("RenewLease failed: %v", err)
	}

	// 2. Lease renewal fails for stale/wrong worker
	if err := store.RenewLease(ctx, cj.ID, "worker-B", *cj.ClaimToken, newLease); !errors.Is(err, jobs.ErrOwnershipLost) {
		t.Fatalf("expected ErrOwnershipLost for wrong worker, got %v", err)
	}

	// 3. Lease renewal fails for wrong claim token
	if err := store.RenewLease(ctx, cj.ID, "worker-A", "stale-token", newLease); !errors.Is(err, jobs.ErrOwnershipLost) {
		t.Fatalf("expected ErrOwnershipLost for wrong token, got %v", err)
	}

	// 4. Stale finalize rejected
	if err := store.ConditionalFinalizeSuccess(ctx, cj.ID, "worker-A", "stale-token"); !errors.Is(err, jobs.ErrOwnershipLost) {
		t.Fatalf("expected ErrOwnershipLost for stale finalize, got %v", err)
	}

	// 5. Authoritative finalize succeeds
	if err := store.ConditionalFinalizeSuccess(ctx, cj.ID, "worker-A", *cj.ClaimToken); err != nil {
		t.Fatalf("authoritative finalize failed: %v", err)
	}

	// Check final state
	finalJob, err := store.GetJobByID(ctx, cj.ID)
	if err != nil {
		t.Fatalf("GetJobByID failed: %v", err)
	}
	if finalJob.Status != jobs.StatusSucceeded || finalJob.FinishedAt == nil {
		t.Fatalf("expected SUCCEEDED status with finished_at set")
	}
}

func TestStore_ExpiredLeaseCannotBeRenewed(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	store := jobs.NewStoreWithDriver(db, "sqlite3")
	ctx := context.Background()
	job := &jobs.AnalysisJob{JobType: jobs.JobTypeMaterializeSnapshot, ResourceID: "expired-lease"}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}
	claimed, err := store.ClaimJobs(ctx, "worker-A", 1, 5*time.Second)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("failed claiming job: %v", err)
	}
	if _, err := db.Exec(`UPDATE analysis_jobs SET lease_until = datetime('now', '-1 second') WHERE id = ?`, claimed[0].ID); err != nil {
		t.Fatalf("expire job lease: %v", err)
	}
	if err := store.RenewLease(ctx, claimed[0].ID, "worker-A", *claimed[0].ClaimToken, time.Now().UTC().Add(time.Minute)); !errors.Is(err, jobs.ErrOwnershipLost) {
		t.Fatalf("expected expired lease renewal to lose ownership, got %v", err)
	}
}

func TestStore_ReaperAndRetryExhaustion(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	store := jobs.NewStoreWithDriver(db, "sqlite3")
	ctx := context.Background()

	job := &jobs.AnalysisJob{
		JobType:     jobs.JobTypeBuildCodeIndex,
		ResourceID:  "code-build-1",
		MaxAttempts: 2,
	}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}

	// First claim
	claimed, err := store.ClaimJobs(ctx, "worker-1", 1, time.Millisecond)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("Claim failed: %v", err)
	}

	// Wait for lease to expire
	time.Sleep(10 * time.Millisecond)

	// Reaper runs
	reaped, err := store.ReapExpiredJobs(ctx, 10)
	if err != nil {
		t.Fatalf("ReapExpiredJobs failed: %v", err)
	}
	if reaped != 1 {
		t.Fatalf("expected 1 reaped job, got %d", reaped)
	}

	reapedJob, _ := store.GetJobByID(ctx, job.ID)
	if reapedJob.Status != jobs.StatusRetryWait {
		t.Fatalf("expected RETRY_WAIT after first expire, got %s", reapedJob.Status)
	}

	// Force next_run_at to past so it can be claimed again
	_, _ = db.Exec("UPDATE analysis_jobs SET next_run_at = datetime('now', '-1 minute') WHERE id = ?", job.ID)

	// Second claim (attempt 2 of 2)
	claimed2, err := store.ClaimJobs(ctx, "worker-2", 1, time.Millisecond)
	if err != nil || len(claimed2) != 1 {
		t.Fatalf("Second claim failed: %v", err)
	}
	if claimed2[0].AttemptCount != 2 {
		t.Fatalf("expected attempt count 2, got %d", claimed2[0].AttemptCount)
	}

	time.Sleep(10 * time.Millisecond)

	// Reaper runs again -> attempt_count (2) >= max_attempts (2) -> terminal FAILED
	reaped2, err := store.ReapExpiredJobs(ctx, 10)
	if err != nil || reaped2 != 1 {
		t.Fatalf("Second reap failed: %v", err)
	}

	exhaustedJob, _ := store.GetJobByID(ctx, job.ID)
	if exhaustedJob.Status != jobs.StatusFailed {
		t.Fatalf("expected FAILED status, got %s", exhaustedJob.Status)
	}
	if exhaustedJob.TerminalReason == nil || *exhaustedJob.TerminalReason != jobs.TerminalReasonRetryableExhausted {
		t.Fatalf("expected terminal reason RETRYABLE_EXHAUSTED, got %v", exhaustedJob.TerminalReason)
	}

	// Test Manual Requeue Rule
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed starting tx: %v", err)
	}
	err = store.ManualRequeueTx(ctx, tx, jobs.JobTypeBuildCodeIndex, "code-build-1")
	if err != nil {
		t.Fatalf("ManualRequeueTx failed: %v", err)
	}
	_ = tx.Commit()

	requeuedJob, _ := store.GetJobByID(ctx, job.ID)
	if requeuedJob.Status != jobs.StatusPending {
		t.Fatalf("expected PENDING status after manual requeue, got %s", requeuedJob.Status)
	}
	if requeuedJob.ExecutionGeneration != 2 {
		t.Fatalf("expected execution generation 2, got %d", requeuedJob.ExecutionGeneration)
	}
	if requeuedJob.AttemptCount != 0 {
		t.Fatalf("expected attempt count reset to 0, got %d", requeuedJob.AttemptCount)
	}
}

func TestStore_ReaperSynchronizesBusinessTerminalState(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()
	for _, ddl := range []string{
		`CREATE TABLE repository_snapshots (id TEXT PRIMARY KEY, status TEXT NOT NULL, error_code TEXT)`,
		`CREATE TABLE code_index_builds (id TEXT PRIMARY KEY, status TEXT NOT NULL, error_code TEXT)`,
		`CREATE TABLE retrieval_builds (id TEXT PRIMARY KEY, status TEXT NOT NULL, error_code TEXT)`,
		`CREATE TABLE diagnosis_runs (id TEXT PRIMARY KEY, status TEXT NOT NULL, final_attempt_id TEXT, version INTEGER NOT NULL)`,
		`CREATE TABLE diagnosis_attempts (id TEXT PRIMARY KEY, diagnosis_run_id TEXT NOT NULL, execution_generation INTEGER NOT NULL, attempt_no INTEGER NOT NULL, status TEXT NOT NULL, finished_at DATETIME, created_at DATETIME)`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	store := jobs.NewStoreWithDriver(db, "sqlite3")
	cases := []struct {
		typ       jobs.JobType
		id, table string
	}{
		{jobs.JobTypeMaterializeSnapshot, "snap-reaped", "repository_snapshots"},
		{jobs.JobTypeBuildCodeIndex, "build-reaped", "code_index_builds"},
		{jobs.JobTypeBuildRetrieval, "retrieval-reaped", "retrieval_builds"},
		{jobs.JobTypeRunDiagnosis, "diag-reaped", "diagnosis_runs"},
	}
	for _, tc := range cases {
		if tc.typ == jobs.JobTypeRunDiagnosis {
			_, _ = db.Exec(`INSERT INTO diagnosis_runs (id,status,version) VALUES (?, 'RUNNING', 1)`, tc.id)
			_, _ = db.Exec(`INSERT INTO diagnosis_attempts (id,diagnosis_run_id,execution_generation,attempt_no,status) VALUES (?, ?, 1, 1, 'RUNNING')`, tc.id+"-attempt", tc.id)
		} else {
			_, _ = db.Exec(`INSERT INTO `+tc.table+` (id,status) VALUES (?, ?)`, tc.id, map[jobs.JobType]string{jobs.JobTypeMaterializeSnapshot: "MATERIALIZING", jobs.JobTypeBuildCodeIndex: "BUILDING", jobs.JobTypeBuildRetrieval: "BUILDING"}[tc.typ])
		}
		job := &jobs.AnalysisJob{JobType: tc.typ, ResourceID: tc.id, MaxAttempts: 1}
		if err := store.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE analysis_jobs SET status='RUNNING', attempt_count=1, worker_id='dead-worker', claim_token='dead-token', lease_until=datetime('now','-1 second') WHERE id=?`, job.ID); err != nil {
			t.Fatal(err)
		}
	}
	if count, err := store.ReapExpiredJobs(ctx, 20); err != nil || count != len(cases) {
		t.Fatalf("reaped=%d err=%v", count, err)
	}
	for _, tc := range cases {
		job, err := store.GetJobByResource(ctx, tc.typ, tc.id)
		if err != nil {
			t.Fatal(err)
		}
		if job.Status != jobs.StatusFailed {
			t.Fatalf("%s job status=%s", tc.typ, job.Status)
		}
		var status string
		if tc.typ == jobs.JobTypeRunDiagnosis {
			var finalAttemptID string
			if err := db.QueryRow(`SELECT status, final_attempt_id FROM diagnosis_runs WHERE id=?`, tc.id).Scan(&status, &finalAttemptID); err != nil {
				t.Fatal(err)
			}
			if finalAttemptID != tc.id+"-attempt" {
				t.Errorf("final_attempt_id=%q, want %q", finalAttemptID, tc.id+"-attempt")
			}
			if status != "FAILED" {
				t.Fatalf("diagnosis status=%s", status)
			}
			if err := db.QueryRow(`SELECT status FROM diagnosis_attempts WHERE diagnosis_run_id=?`, tc.id).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if status != "ABANDONED" {
				t.Fatalf("attempt status=%s", status)
			}
		} else {
			if err := db.QueryRow(`SELECT status FROM `+tc.table+` WHERE id=?`, tc.id).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if status != "FAILED" {
				t.Fatalf("%s business status=%s", tc.typ, status)
			}
		}
	}
}

func TestStore_TerminalStageFailureSynchronizesAnalysisRevision(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()
	for _, ddl := range []string{
		`CREATE TABLE code_index_builds (id TEXT PRIMARY KEY, status TEXT NOT NULL, error_code TEXT)`,
		`CREATE TABLE analysis_revisions (id TEXT PRIMARY KEY, snapshot_id TEXT, code_index_build_id TEXT, retrieval_build_id TEXT, status TEXT NOT NULL, stage TEXT NOT NULL, error_code TEXT, error_message TEXT, version INTEGER NOT NULL, updated_at DATETIME)`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO code_index_builds (id, status) VALUES ('build-terminal', 'BUILDING')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO analysis_revisions (id, code_index_build_id, status, stage, version) VALUES ('revision-terminal', 'build-terminal', 'PREPARING', 'BUILDING_CODE_INDEX', 2)`); err != nil {
		t.Fatal(err)
	}

	store := jobs.NewStoreWithDriver(db, "sqlite3")
	job := &jobs.AnalysisJob{JobType: jobs.JobTypeBuildCodeIndex, ResourceID: "build-terminal", MaxAttempts: 1}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimJobs(ctx, "worker-terminal", 1, time.Second)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim failed: %v", err)
	}
	claim := claimed[0]
	if err := store.ConditionalFinalizeFailure(ctx, claim.ID, "worker-terminal", *claim.ClaimToken, jobs.ErrorClassPermanent, "CODE_INDEX_ANALYSIS_FAILED", "analysis failed", nil, true, time.Time{}); err != nil {
		t.Fatal(err)
	}

	var status, stage, code, message string
	if err := db.QueryRow(`SELECT status, stage, error_code, error_message FROM analysis_revisions WHERE id='revision-terminal'`).Scan(&status, &stage, &code, &message); err != nil {
		t.Fatal(err)
	}
	if status != "FAILED" || stage != "BUILDING_CODE_INDEX" || code != "CODE_INDEX_ANALYSIS_FAILED" || message != "analysis failed" {
		t.Fatalf("revision terminal state = %s/%s/%s/%s", status, stage, code, message)
	}
}

func TestWorkerRuntime_ConcurrentExecution(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	store := jobs.NewStoreWithDriver(db, "sqlite3")
	ctx := context.Background()

	var processedCount int64

	cfg := jobs.DefaultWorkerConfig()
	cfg.Concurrency = 4
	cfg.PollInterval = 20 * time.Millisecond
	cfg.LeaseDuration = 5 * time.Second

	worker := jobs.NewWorker(store, cfg)
	worker.RegisterHandler(jobs.JobTypeRunDiagnosis, jobs.HandlerFunc(func(ctx context.Context, job *jobs.AnalysisJob) error {
		atomic.AddInt64(&processedCount, 1)
		time.Sleep(10 * time.Millisecond)
		return nil
	}))

	// Enqueue 8 jobs
	for i := 1; i <= 8; i++ {
		job := &jobs.AnalysisJob{
			JobType:    jobs.JobTypeRunDiagnosis,
			ResourceID: fmt.Sprintf("diag-batch-%d", i),
		}
		if err := store.CreateJob(ctx, job); err != nil {
			t.Fatalf("CreateJob failed: %v", err)
		}
	}

	worker.Start(ctx)

	// Wait for all 8 jobs to complete
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt64(&processedCount) < 8 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}

	worker.Stop()

	if processed := atomic.LoadInt64(&processedCount); processed != 8 {
		t.Fatalf("expected 8 processed jobs, got %d", processed)
	}
}

func TestWorkerRuntime_DoesNotLeaseJobsBeforeExecutionCapacity(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	store := jobs.NewStoreWithDriver(db, "sqlite3")
	ctx := context.Background()
	cfg := jobs.DefaultWorkerConfig()
	cfg.WorkerID = "worker-capacity-lease"
	cfg.Concurrency = 1
	cfg.BatchSize = 2
	cfg.PollInterval = 10 * time.Millisecond
	cfg.LeaseDuration = 180 * time.Millisecond
	cfg.ReapInterval = 10 * time.Millisecond
	worker := jobs.NewWorker(store, cfg)

	first := &jobs.AnalysisJob{JobType: jobs.JobTypeRunDiagnosis, ResourceID: "capacity-first", MaxAttempts: 3}
	second := &jobs.AnalysisJob{JobType: jobs.JobTypeRunDiagnosis, ResourceID: "capacity-second", MaxAttempts: 3}
	for _, job := range []*jobs.AnalysisJob{first, second} {
		if err := store.CreateJob(ctx, job); err != nil {
			t.Fatalf("CreateJob(%s): %v", job.ResourceID, err)
		}
	}

	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{}, 1)
	releaseFirst := make(chan struct{})
	var active int32
	var maxActive int32
	worker.RegisterHandler(jobs.JobTypeRunDiagnosis, jobs.HandlerFunc(func(ctx context.Context, job *jobs.AnalysisJob) error {
		current := atomic.AddInt32(&active, 1)
		for previous := atomic.LoadInt32(&maxActive); current > previous; previous = atomic.LoadInt32(&maxActive) {
			if atomic.CompareAndSwapInt32(&maxActive, previous, current) {
				break
			}
		}
		defer atomic.AddInt32(&active, -1)

		if job.ResourceID == first.ResourceID {
			close(firstStarted)
			select {
			case <-releaseFirst:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		secondStarted <- struct{}{}
		return nil
	}))
	worker.Start(ctx)
	defer func() {
		select {
		case <-releaseFirst:
		default:
			close(releaseFirst)
		}
		worker.Stop()
	}()

	select {
	case <-firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first job did not acquire execution capacity")
	}

	// Keep the first handler beyond multiple lease periods while the only
	// execution slot is occupied. The second job must remain unclaimed rather
	// than aging a lease while waiting outside execution capacity.
	time.Sleep(3 * cfg.LeaseDuration)
	queued, err := store.GetJobByID(ctx, second.ID)
	if err != nil {
		t.Fatalf("GetJobByID(second): %v", err)
	}
	if queued.Status != jobs.StatusPending || queued.AttemptCount != 0 {
		t.Fatalf("waiting job state = %s with attempt_count=%d, want PENDING with no consumed attempt", queued.Status, queued.AttemptCount)
	}
	running, err := store.GetJobByID(ctx, first.ID)
	if err != nil {
		t.Fatalf("GetJobByID(first): %v", err)
	}
	if running.Status != jobs.StatusRunning || running.AttemptCount != 1 {
		t.Fatalf("active job state = %s with attempt_count=%d, want RUNNING/1", running.Status, running.AttemptCount)
	}

	close(releaseFirst)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		completedFirst, firstErr := store.GetJobByID(ctx, first.ID)
		completedSecond, secondErr := store.GetJobByID(ctx, second.ID)
		if firstErr != nil || secondErr != nil {
			t.Fatalf("read completed jobs: first=%v second=%v", firstErr, secondErr)
		}
		if completedFirst.Status == jobs.StatusSucceeded && completedSecond.Status == jobs.StatusSucceeded {
			if completedFirst.AttemptCount != 1 || completedSecond.AttemptCount != 1 {
				t.Fatalf("attempt counts = first %d / second %d, want 1 / 1", completedFirst.AttemptCount, completedSecond.AttemptCount)
			}
			if got := atomic.LoadInt32(&maxActive); got > 1 {
				t.Fatalf("max concurrent handlers = %d, want <= 1", got)
			}
			select {
			case <-secondStarted:
			default:
				t.Fatal("second job reached SUCCEEDED without executing")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("both jobs did not complete after execution capacity became available")
}

func TestWorkerRuntime_GracefulShutdownDrainsInFlightJob(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	store := jobs.NewStoreWithDriver(db, "sqlite3")
	ctx := context.Background()

	cfg := jobs.DefaultWorkerConfig()
	cfg.PollInterval = 20 * time.Millisecond

	worker := jobs.NewWorker(store, cfg)

	startedCh := make(chan struct{})
	finishJobCh := make(chan struct{})
	worker.RegisterHandler(jobs.JobTypeMaterializeSnapshot, jobs.HandlerFunc(func(ctx context.Context, job *jobs.AnalysisJob) error {
		close(startedCh)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-finishJobCh:
			return nil
		}
	}))

	job := &jobs.AnalysisJob{
		JobType:    jobs.JobTypeMaterializeSnapshot,
		ResourceID: "snap-cancel-1",
	}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}

	workerCtx, cancelWorker := context.WithCancel(ctx)
	worker.Start(workerCtx)

	// Wait for job handler to start
	<-startedCh

	// cmd/worker's SIGTERM path must stop claiming and drain before cancelling
	// the shared root context. Keep Stop blocked while the in-flight handler is
	// active, then let the job complete as normal work.
	stopped := make(chan struct{})
	go func() {
		worker.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("worker stopped before the in-flight job completed")
	case <-time.After(100 * time.Millisecond):
	}
	close(finishJobCh)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not finish draining the in-flight job")
	}
	// Mirror cmd/worker: the process root is cancelled only after drain.
	cancelWorker()

	completedJob, err := store.GetJobByID(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJobByID failed: %v", err)
	}
	if completedJob.Status != jobs.StatusSucceeded {
		t.Fatalf("expected graceful shutdown to preserve job completion, got %s", completedJob.Status)
	}
}

func TestWorkerRuntime_UserCancellationStillCancelsJob(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	store := jobs.NewStoreWithDriver(db, "sqlite3")
	ctx := context.Background()

	cfg := jobs.DefaultWorkerConfig()
	cfg.PollInterval = 20 * time.Millisecond

	worker := jobs.NewWorker(store, cfg)
	startedCh := make(chan struct{})
	worker.RegisterHandler(jobs.JobTypeMaterializeSnapshot, jobs.HandlerFunc(func(ctx context.Context, job *jobs.AnalysisJob) error {
		close(startedCh)
		<-ctx.Done()
		return ctx.Err()
	}))

	job := &jobs.AnalysisJob{JobType: jobs.JobTypeMaterializeSnapshot, ResourceID: "snap-user-cancel-1"}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}
	worker.Start(ctx)
	defer worker.Stop()

	select {
	case <-startedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("job handler did not start")
	}
	if err := store.RequestCancel(ctx, job.JobType, job.ResourceID); err != nil {
		t.Fatalf("RequestCancel failed: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cancelledJob, err := store.GetJobByID(ctx, job.ID)
		if err != nil {
			t.Fatalf("GetJobByID failed: %v", err)
		}
		if cancelledJob.Status == jobs.StatusCancelled {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("user cancellation did not finalize the job as CANCELLED")
}
