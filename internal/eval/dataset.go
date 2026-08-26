package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"repolens/internal/indexing"
)

var StandardFaultCases = []EvalCase{
	{
		CaseID:            "CASE-001",
		DatasetVersion:    "v1.0",
		RepositoryName:    "repolens/payment-service",
		SnapshotSHA:       "snap-pay-01",
		IssueTitle:        "Nil pointer dereference in payment handler initialization",
		IssueDescription:  "Service panics on startup when DB_DSN environment variable is empty.",
		ErrorLog:          "panic: runtime error: invalid memory address or nil pointer dereference [signal SIGSEGV: code=0x1 addr=0x0 pc=0x7f23]",
		ExpectedRootCause: "Nil pointer dereference when initializing database connection with unpopulated config struct",
		RelevantFiles:     []string{"internal/platform/config/config.go", "internal/platform/mysql/mysql.go"},
		ForbiddenClaims:   []string{"hardware memory corruption", "network switch failure"},
	},
	{
		CaseID:            "CASE-002",
		DatasetVersion:    "v1.0",
		RepositoryName:    "repolens/auth-service",
		SnapshotSHA:       "snap-auth-02",
		IssueTitle:        "JWT validation returns 401 for valid tokens after midnight",
		IssueDescription:  "Users are logged out unexpectedly every night at 00:00 UTC.",
		ErrorLog:          "auth error: token expired or clock skew exceeded: issued_at in future",
		ExpectedRootCause: "Time zone discrepancy in token issued_at calculation comparing UTC with local timestamp",
		RelevantFiles:     []string{"internal/auth/jwt.go", "internal/auth/middleware.go"},
		ForbiddenClaims:   []string{"secret key leakage", "database down"},
	},
	{
		CaseID:            "CASE-003",
		DatasetVersion:    "v1.0",
		RepositoryName:    "repolens/order-service",
		SnapshotSHA:       "snap-order-03",
		IssueTitle:        "Duplicate orders created on rapid client double clicks",
		IssueDescription:  "Clients submitting two identical checkout requests within 100ms receive two order IDs.",
		ErrorLog:          "order duplicate warning: duplicate checkout triggered without unique constraint check",
		ExpectedRootCause: "Missing unique idempotency key constraint and request hash verification in database transaction",
		RelevantFiles:     []string{"internal/diagnosis/store.go", "internal/diagnosis/service.go"},
		ForbiddenClaims:   []string{"client browser bug"},
	},
	{
		CaseID:            "CASE-004",
		DatasetVersion:    "v1.0",
		RepositoryName:    "repolens/queue-service",
		SnapshotSHA:       "snap-queue-04",
		IssueTitle:        "RabbitMQ consumer enters hot loop on LLM 429 rate limit error",
		IssueDescription:  "When LLM API responds with 429, CPU spikes to 100% and millions of messages are redelivered instantly.",
		ErrorLog:          "worker error: rate limit 429 exceeded, immediate nack with requeue=true executed",
		ExpectedRootCause: "Immediate nack with requeue instead of transitioning run to RETRY_WAIT with delayed outbox event",
		RelevantFiles:     []string{"internal/worker/consumer.go", "internal/outbox/store.go"},
		ForbiddenClaims:   []string{"RabbitMQ server crashed"},
	},
	{
		CaseID:            "CASE-005",
		DatasetVersion:    "v1.0",
		RepositoryName:    "repolens/queue-worker",
		SnapshotSHA:       "snap-worker-05",
		IssueTitle:        "Diagnosis tasks stuck in RUNNING forever after worker pod kill",
		IssueDescription:  "SIGKILL sent to worker pod causes tasks to remain in RUNNING status perpetually.",
		ErrorLog:          "run timeout: task #4928 remains in RUNNING status for 4 hours without heartbeat",
		ExpectedRootCause: "Missing stale attempt recovery sweeper to mark dead worker attempts as ABANDONED and trigger retry",
		RelevantFiles:     []string{"internal/worker/recovery.go", "internal/worker/consumer.go"},
		ForbiddenClaims:   []string{"MySQL table corruption"},
	},
	{
		CaseID:            "CASE-006",
		DatasetVersion:    "v1.0",
		RepositoryName:    "repolens/git-ingest",
		SnapshotSHA:       "snap-git-06",
		IssueTitle:        "SSRF vulnerability allows cloning internal cloud metadata endpoint",
		IssueDescription:  "User passed http://169.254.169.254/latest/meta-data as git URL.",
		ErrorLog:          "security alert: outbound request attempted to cloud metadata link-local address 169.254.169.254",
		ExpectedRootCause: "Git cloner lacks host allowlist and IP resolution check for private and link-local CIDR blocks",
		RelevantFiles:     []string{"internal/indexing/clone.go"},
		ForbiddenClaims:   []string{"AWS IAM permission error"},
	},
	{
		CaseID:            "CASE-007",
		DatasetVersion:    "v1.0",
		RepositoryName:    "repolens/file-reader",
		SnapshotSHA:       "snap-file-07",
		IssueTitle:        "Path traversal in read_file tool exposes host /etc/passwd",
		IssueDescription:  "Agent attempted to execute read_file with path ../../../../etc/passwd.",
		ErrorLog:          "security error: attempt to read file outside snapshot source root directory",
		ExpectedRootCause: "Missing path cleanliness check and symlink evaluation against repository snapshot root",
		RelevantFiles:     []string{"internal/tools/read_file.go", "internal/platform/snapshotstore/snapshotstore.go"},
		ForbiddenClaims:   []string{"Linux kernel filesystem bug"},
	},
	{
		CaseID:            "CASE-008",
		DatasetVersion:    "v1.0",
		RepositoryName:    "repolens/agent-core",
		SnapshotSHA:       "snap-agent-08",
		IssueTitle:        "Agent enters infinite loop calling search_code with identical query",
		IssueDescription:  "LLM generated 50 consecutive calls to search_code('test') exhausting token budget.",
		ErrorLog:          "agent loop error: max steps exceeded due to repeated identical tool invocation",
		ExpectedRootCause: "Missing repeat-call hash detection guard to abort consecutive identical tool executions",
		RelevantFiles:     []string{"internal/agent/guard.go", "internal/agent/loop.go"},
		ForbiddenClaims:   []string{"database deadlock"},
	},
	{
		CaseID:            "CASE-009",
		DatasetVersion:    "v1.0",
		RepositoryName:    "repolens/citation-engine",
		SnapshotSHA:       "snap-cit-09",
		IssueTitle:        "Hallucinated citations pass through to client report without verification",
		IssueDescription:  "Report contains citation for non-existent file utils/helper.go line 999.",
		ErrorLog:          "citation mismatch: file utils/helper.go does not exist in target snapshot",
		ExpectedRootCause: "Backend failed to validate citations against immutable snapshot filesystem before persisting report",
		RelevantFiles:     []string{"internal/evidence/citation.go"},
		ForbiddenClaims:   []string{"git checkout failed"},
	},
	{
		CaseID:            "CASE-010",
		DatasetVersion:    "v1.0",
		RepositoryName:    "repolens/sse-streamer",
		SnapshotSHA:       "snap-sse-10",
		IssueTitle:        "Client reconnecting to SSE stream loses previous step history",
		IssueDescription:  "When network blips, reconnecting client does not see steps 1 through 5.",
		ErrorLog:          "sse replay error: client sent Last-Event-ID: 3 but received only new events",
		ExpectedRootCause: "SSE handler does not query AgentStep records from database filtered by Last-Event-ID on connection",
		RelevantFiles:     []string{"internal/sse/handler.go", "internal/trace/step.go"},
		ForbiddenClaims:   []string{"HTTP/2 stream multiplexing broken"},
	},
	{
		CaseID:            "CASE-011",
		DatasetVersion:    "v1.0",
		RepositoryName:    "repolens/payment-service",
		SnapshotSHA:       "snap-db-11",
		IssueTitle:        "Database connection pool exhausted under concurrent diagnosis requests",
		IssueDescription:  "HTTP requests block indefinitely and return 504 Gateway Timeout during load spike.",
		ErrorLog:          "mysql error: Error 1040: Too many connections / connection pool full",
		ExpectedRootCause: "Goroutines leaking sql.DB connections due to missing defer rows.Close()",
		RelevantFiles:     []string{"internal/platform/mysql/mysql.go", "internal/platform/config/config.go"},
		ForbiddenClaims:   []string{"DNS server unreachable"},
	},
	{
		CaseID:            "CASE-012",
		DatasetVersion:    "v1.0",
		RepositoryName:    "repolens/agent-runtime",
		SnapshotSHA:       "snap-conc-12",
		IssueTitle:        "Concurrent map read and map write crash in telemetry reporter",
		IssueDescription:  "Application crashes intermittently with fatal error: concurrent map writes.",
		ErrorLog:          "fatal error: concurrent map iteration and map write in metrics collector",
		ExpectedRootCause: "Shared map accessed across goroutines without sync.RWMutex synchronization",
		RelevantFiles:     []string{"internal/retrieval/lexical.go", "internal/sse/handler.go"},
		ForbiddenClaims:   []string{"RAM hardware bit flip"},
	},
	{
		CaseID:            "CASE-013",
		DatasetVersion:    "v1.0",
		RepositoryName:    "repolens/http-client",
		SnapshotSHA:       "snap-http-13",
		IssueTitle:        "HTTP client socket leak leads to too many open files error",
		IssueDescription:  "After 10,000 requests, server errors with socket: too many open files.",
		ErrorLog:          "http error: dial tcp: socket: too many open files",
		ExpectedRootCause: "Missing resp.Body.Close() on HTTP client response causing socket descriptors to remain open",
		RelevantFiles:     []string{"internal/llm/openai_compatible.go"},
		ForbiddenClaims:   []string{"kernel ulimit hardcoded to 10"},
	},
	{
		CaseID:            "CASE-014",
		DatasetVersion:    "v1.0",
		RepositoryName:    "repolens/git-ingest",
		SnapshotSHA:       "snap-chunk-14",
		IssueTitle:        "Index out of range panic during code chunk slicing with overlap",
		IssueDescription:  "Single-line files cause code chunker to panic on index out of range [0:60].",
		ErrorLog:          "panic: runtime error: slice bounds out of range [:60] with capacity 1",
		ExpectedRootCause: "Chunker does not clamp end line boundary to total file line count",
		RelevantFiles:     []string{"internal/indexing/chunk.go"},
		ForbiddenClaims:   []string{"text file encoding error"},
	},
	{
		CaseID:            "CASE-015",
		DatasetVersion:    "v1.0",
		RepositoryName:    "repolens/order-service",
		SnapshotSHA:       "snap-relay-15",
		IssueTitle:        "Outbox events processed out of order causing race in status update",
		IssueDescription:  "RETRY_REQUESTED event dispatched before DIAGNOSIS_REQUESTED completed.",
		ErrorLog:          "order conflict: attempt to transition run from SUCCEEDED to RETRY_WAIT",
		ExpectedRootCause: "Outbox relay fetching without ordering by available_at ASC or lack of conditional update check",
		RelevantFiles:     []string{"internal/diagnosis/state.go", "internal/diagnosis/store.go"},
		ForbiddenClaims:   []string{"RabbitMQ clustering failure"},
	},
	{
		CaseID:            "CASE-016",
		DatasetVersion:    "v1.0",
		RepositoryName:    "repolens/order-service",
		SnapshotSHA:       "snap-tx-16",
		IssueTitle:        "DiagnosisRun created in DB but OutboxEvent lost on crash",
		IssueDescription:  "Task is listed in database as QUEUED but never picked up by RabbitMQ worker.",
		ErrorLog:          "audit discrepancy: DiagnosisRun exists without corresponding OutboxEvent record",
		ExpectedRootCause: "DiagnosisRun and OutboxEvent were inserted in separate database transactions instead of a single atomic transaction",
		RelevantFiles:     []string{"internal/diagnosis/store.go", "internal/diagnosis/service.go"},
		ForbiddenClaims:   []string{"RabbitMQ deleted the message"},
	},
	{
		CaseID:            "CASE-017",
		DatasetVersion:    "v1.0",
		RepositoryName:    "repolens/agent-runtime",
		SnapshotSHA:       "snap-cancel-17",
		IssueTitle:        "Cancelled diagnosis continues executing LLM calls and wasting tokens",
		IssueDescription:  "User called /cancel but worker continued processing 6 more agent steps.",
		ErrorLog:          "cancellation delay: worker completed execution 40 seconds after cancel_requested set to true",
		ExpectedRootCause: "Agent loop did not check ctx.Done() and DB CancelRequested flag between tool steps",
		RelevantFiles:     []string{"internal/agent/loop.go", "internal/agent/guard.go"},
		ForbiddenClaims:   []string{"Linux kill signal failure"},
	},
	{
		CaseID:            "CASE-018",
		DatasetVersion:    "v1.0",
		RepositoryName:    "repolens/bm25-ranker",
		SnapshotSHA:       "snap-bm25-18",
		IssueTitle:        "BM25 ranker gives zero score to camelCase identifiers in query",
		IssueDescription:  "Searching for 'handleRequest' fails to match 'HandleRequest' function symbol.",
		ErrorLog:          "retrieval miss: zero candidates returned for camelCase method name",
		ExpectedRootCause: "Tokenizer did not split camelCase and normalize terms to lowercase during index and query parsing",
		RelevantFiles:     []string{"internal/retrieval/bm25.go"},
		ForbiddenClaims:   []string{"Elasticsearch cluster down"},
	},
	{
		CaseID:            "CASE-019",
		DatasetVersion:    "v1.0",
		RepositoryName:    "repolens/order-service",
		SnapshotSHA:       "snap-secret-19",
		IssueTitle:        "AWS secret access key in error log leaked to external LLM provider",
		IssueDescription:  "CI log containing AKIA... was sent unredacted in LLM prompt payload.",
		ErrorLog:          "security violation: unredacted credential pattern detected in outbound LLM payload",
		ExpectedRootCause: "Missing regex secret redaction filter on IssueDescription and ErrorLog before saving and prompt construction",
		RelevantFiles:     []string{"internal/diagnosis/service.go"},
		ForbiddenClaims:   []string{"AWS compromised"},
	},
	{
		CaseID:            "CASE-020",
		DatasetVersion:    "v1.0",
		RepositoryName:    "repolens/rrf-fusion",
		SnapshotSHA:       "snap-rrf-20",
		IssueTitle:        "Hybrid search returns duplicate documents with varying score scales",
		IssueDescription:  "Results list contains identical file chunk twice with raw BM25 score and raw cosine similarity.",
		ErrorLog:          "ranking anomaly: incompatible raw scores merged directly without rank reciprocal transformation",
		ExpectedRootCause: "Direct addition of raw BM25 and vector scores instead of Reciprocal Rank Fusion formula",
		RelevantFiles:     []string{"internal/retrieval/rrf.go"},
		ForbiddenClaims:   []string{"vector index corrupted"},
	},
	{
		CaseID:            "CASE-021",
		DatasetVersion:    "v1.0",
		RepositoryName:    "repolens/queue-worker",
		SnapshotSHA:       "snap-shut-21",
		IssueTitle:        "Worker terminates abruptly on SIGTERM discarding in-flight task progress",
		IssueDescription:  "K8s rolling update causes running tasks to fail abruptly without updating attempt status.",
		ErrorLog:          "process exit: SIGTERM received, process terminated immediately without waiting for in-flight tasks",
		ExpectedRootCause: "Missing graceful shutdown coordinator waiting on sync.WaitGroup within shutdown timeout",
		RelevantFiles:     []string{"internal/worker/consumer.go", "internal/worker/recovery.go"},
		ForbiddenClaims:   []string{"hardware power outage"},
	},
	{
		CaseID:            "CASE-022",
		DatasetVersion:    "v1.0",
		RepositoryName:    "repolens/json-parser",
		SnapshotSHA:       "snap-json-22",
		IssueTitle:        "Markdown code fence in LLM response causes JSON unmarshal failure",
		IssueDescription:  "LLM output starting with ```json is rejected as invalid report format.",
		ErrorLog:          "parse error: invalid character '`' looking for beginning of value",
		ExpectedRootCause: "Missing markdown code fence stripping and regex fallback in report parser",
		RelevantFiles:     []string{"internal/agent/loop.go"},
		ForbiddenClaims:   []string{"LLM model deleted"},
	},
	{
		CaseID:            "CASE-023",
		DatasetVersion:    "v1.0",
		RepositoryName:    "repolens/order-service",
		SnapshotSHA:       "snap-opt-23",
		IssueTitle:        "Concurrent workers claim the same DiagnosisRun causing lost update",
		IssueDescription:  "Two workers process Run #100 simultaneously generating two separate reports.",
		ErrorLog:          "concurrency anomaly: duplicate Attempt #1 created by worker-a and worker-b",
		ExpectedRootCause: "Missing version conditional update (WHERE id = ? AND version = ?) during Worker claim transaction",
		RelevantFiles:     []string{"internal/diagnosis/store.go", "internal/diagnosis/state.go"},
		ForbiddenClaims:   []string{"MySQL isolation level set to READ UNCOMMITTED"},
	},
	{
		CaseID:            "CASE-024",
		DatasetVersion:    "v1.0",
		RepositoryName:    "repolens/git-ingest",
		SnapshotSHA:       "snap-filter-24",
		IssueTitle:        "Indexer indexes binary .so files and node_modules filling storage",
		IssueDescription:  "Repository indexing consumes 2GB memory indexing node_modules and binary shared objects.",
		ErrorLog:          "indexer warning: binary content and third-party dependencies ingested into chunk store",
		ExpectedRootCause: "FileFilter missing ignored directory rules for node_modules and binary extensions",
		RelevantFiles:     []string{"internal/indexing/filter.go"},
		ForbiddenClaims:   []string{"SSD disk corrupted"},
	},
	{
		CaseID:            "CASE-025",
		DatasetVersion:    "v1.0",
		RepositoryName:    "repolens/agent-runtime",
		SnapshotSHA:       "snap-eval-25",
		IssueTitle:        "Eval metrics differ between runs without record of changed prompt version",
		IssueDescription:  "Regression score dropped from 92% to 74% but unable to trace which component changed.",
		ErrorLog:          "eval audit missing: EvalRun record lacks model, prompt_version and retrieval_version metadata",
		ExpectedRootCause: "EvalRun struct did not record full reproducibility parameters (dataset_version, git_commit, prompt_version)",
		RelevantFiles:     []string{"internal/evidence/report.go", "pkg/prompt/templates.go"},
		ForbiddenClaims:   []string{"CPU instruction set changed"},
	},
	{
		CaseID:            "CASE-026",
		DatasetVersion:    "v1.0",
		RepositoryName:    "repolens/agent-runtime",
		SnapshotSHA:       "snap-chan-26",
		IssueTitle:        "Deadlock on unbuffered channel write when consumer exits early",
		IssueDescription:  "Goroutine leaks and blocks forever writing to unbuffered channel.",
		ErrorLog:          "goroutine leak: goroutine stuck in chan send (select) without active receiver",
		ExpectedRootCause: "Writing to unbuffered channel without select default or buffered channel capacity",
		RelevantFiles:     []string{"internal/sse/handler.go", "internal/trace/step.go"},
		ForbiddenClaims:   []string{"OS kernel deadlocked"},
	},
	{
		CaseID:            "CASE-027",
		DatasetVersion:    "v1.0",
		RepositoryName:    "repolens/symlink-guard",
		SnapshotSHA:       "snap-sym-27",
		IssueTitle:        "Symlink inside repository pointing to /var/log read by read_file",
		IssueDescription:  "Attacker created symlink link.txt -> /var/log/syslog in repository.",
		ErrorLog:          "security violation: symlink target /var/log/syslog escapes snapshot root directory",
		ExpectedRootCause: "Missing filepath.EvalSymlinks verification comparing resolved path against snapshot root",
		RelevantFiles:     []string{"internal/platform/snapshotstore/snapshotstore.go", "internal/tools/read_file.go"},
		ForbiddenClaims:   []string{"filesystem driver bug"},
	},
	{
		CaseID:            "CASE-028",
		DatasetVersion:    "v1.0",
		RepositoryName:    "repolens/queue-worker",
		SnapshotSHA:       "snap-dlq-28",
		IssueTitle:        "Poison message with corrupted bytes repeatedly crashes consumer",
		IssueDescription:  "Invalid JSON payload causes worker to crash on every restart.",
		ErrorLog:          "unmarshal failure: unexpected EOF parsing message payload",
		ExpectedRootCause: "Worker did not catch JSON unmarshal errors to reject message to DLQ (QueueDiagnosisDLQ)",
		RelevantFiles:     []string{"internal/worker/consumer.go", "internal/outbox/store.go"},
		ForbiddenClaims:   []string{"RabbitMQ corrupted queues"},
	},
	{
		CaseID:            "CASE-029",
		DatasetVersion:    "v1.0",
		RepositoryName:    "repolens/order-service",
		SnapshotSHA:       "snap-idemp-29",
		IssueTitle:        "Idempotency key reused with different issue payload returns 200 instead of 409",
		IssueDescription:  "Client reused Idempotency-Key: abc for a completely different repository and issue.",
		ErrorLog:          "idempotency error: key abc submitted with mismatched request hash",
		ExpectedRootCause: "Service did not compute SHA256 request hash and return 409 Conflict when hash differs",
		RelevantFiles:     []string{"internal/diagnosis/service.go", "internal/diagnosis/store.go"},
		ForbiddenClaims:   []string{"HTTP proxy altered header"},
	},
	{
		CaseID:            "CASE-030",
		DatasetVersion:    "v1.0",
		RepositoryName:    "repolens/queue-worker",
		SnapshotSHA:       "snap-hb-30",
		IssueTitle:        "Slow LLM response blocks heartbeat emitter causing premature task recovery",
		IssueDescription:  "LLM call taking 45s causes recovery sweeper to mark attempt ABANDONED while still running.",
		ErrorLog:          "stale attempt warning: worker heartbeat expired after 30s threshold during long LLM call",
		ExpectedRootCause: "Heartbeat ticker ran in the same goroutine as synchronous LLM request instead of dedicated background goroutine",
		RelevantFiles:     []string{"internal/worker/recovery.go", "internal/worker/consumer.go"},
		ForbiddenClaims:   []string{"system clock jumped forward"},
	},
	{
		CaseID:            "CASE-031",
		DatasetVersion:    "v1.0",
		RepositoryName:    "repolens/citation-range",
		SnapshotSHA:       "snap-range-31",
		IssueTitle:        "Invalid citation line range (start > end) accepted without error",
		IssueDescription:  "Report citations with start_line=50 and end_line=20 were marked as VALID.",
		ErrorLog:          "citation validation error: invalid line range 50 to 20",
		ExpectedRootCause: "Citation validator did not verify startLine >= 1 and endLine >= startLine",
		RelevantFiles:     []string{"internal/evidence/citation.go"},
		ForbiddenClaims:   []string{"IDE line numbering bug"},
	},
	{
		CaseID:            "CASE-032",
		DatasetVersion:    "v1.0",
		RepositoryName:    "repolens/queue-worker",
		SnapshotSHA:       "snap-retry-32",
		IssueTitle:        "Retryable errors retried endlessly beyond MaxAttempts limit",
		IssueDescription:  "Task with persistent 500 error reached Attempt #15 instead of failing at Attempt #3.",
		ErrorLog:          "retry loop: attempt #15 spawned for diagnosis run #894",
		ExpectedRootCause: "Worker consumer did not check attempt.AttemptNo >= MaxAttempts before scheduling retry outbox event",
		RelevantFiles:     []string{"internal/worker/consumer.go"},
		ForbiddenClaims:   []string{"RabbitMQ auto-retry bug"},
	},
}

func WriteStandardDatasetToDir(dir string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	for _, c := range StandardFaultCases {
		data, err := json.MarshalIndent(c, "", "  ")
		if err != nil {
			return err
		}
		path := filepath.Join(dir, fmt.Sprintf("%s.json", c.CaseID))
		if err := os.WriteFile(path, data, 0644); err != nil {
			return err
		}
	}
	return nil
}

// WriteDatasetSets materializes the frozen split used by the v2.1 eval
// protocol. Cases 001-016 are development cases; 017-032 are held-out.
func WriteDatasetSets(baseDir string) error {
	dev := filepath.Join(baseDir, "dev")
	if err := os.MkdirAll(dev, 0755); err != nil {
		return err
	}
	for _, c := range StandardFaultCases {
		if c.CaseID >= "CASE-017" {
			continue
		}
		if err := writeCaseFile(dev, c); err != nil {
			return err
		}
	}
	// Keep the held-out directory immutable after first materialization.
	heldout := filepath.Join(baseDir, "heldout")
	if err := os.MkdirAll(heldout, 0755); err != nil {
		return err
	}
	for _, c := range StandardFaultCases {
		if c.CaseID < "CASE-017" {
			continue
		}
		path := filepath.Join(heldout, fmt.Sprintf("%s.json", c.CaseID))
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if err := writeCaseFile(heldout, c); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeCaseFile(dir string, c EvalCase) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s.json", c.CaseID)), data, 0644)
}

func findFixtureBaseDir() string {
	candidates := []string{
		"testdata/eval_repositories",
		"../../testdata/eval_repositories",
		"../testdata/eval_repositories",
		"/home/lls/projects/repolens/testdata/eval_repositories",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return "testdata/eval_repositories"
}

func GetFixturePathForRepo(repoName string) string {
	base := findFixtureBaseDir()
	var sub string
	switch {
	case strings.Contains(repoName, "payment"):
		sub = "payment_service"
	case strings.Contains(repoName, "auth"):
		sub = "auth_service"
	case strings.Contains(repoName, "order"):
		sub = "order_service"
	case strings.Contains(repoName, "queue") || strings.Contains(repoName, "worker") || strings.Contains(repoName, "pool") || strings.Contains(repoName, "retry") || strings.Contains(repoName, "relay"):
		sub = "queue_worker"
	case strings.Contains(repoName, "git") || strings.Contains(repoName, "chunk") || strings.Contains(repoName, "filter"):
		sub = "git_ingest"
	default:
		sub = "agent_runtime"
	}
	return filepath.Join(base, sub)
}

func LoadFixtureChunksAndSnapshot(fixtureRoot, targetSnapshotDir string, snapshotSHA string, chunker *indexing.CodeChunker) ([]indexing.CodeChunk, error) {
	var chunks []indexing.CodeChunk
	err := filepath.Walk(fixtureRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		relPath, err := filepath.Rel(fixtureRoot, path)
		if err != nil {
			return nil
		}
		contentBytes, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		content := string(contentBytes)
		fileChunks := chunker.ChunkFile(snapshotSHA, relPath, content)
		chunks = append(chunks, fileChunks...)

		if targetSnapshotDir != "" {
			destPath := filepath.Join(targetSnapshotDir, relPath)
			_ = os.MkdirAll(filepath.Dir(destPath), 0755)
			_ = os.WriteFile(destPath, contentBytes, 0644)
		}
		return nil
	})
	return chunks, err
}

func ValidateDatasetFixtures(cases []EvalCase) error {
	baseDir := findFixtureBaseDir()
	info, err := os.Stat(baseDir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("fixture ground truth validation failed: fixture base dir %q not accessible: %w", baseDir, err)
	}

	if len(cases) == 0 {
		return fmt.Errorf("fixture ground truth validation failed: case list is empty")
	}

	var validationErrors []string

	for _, c := range cases {
		if c.CaseID == "" {
			validationErrors = append(validationErrors, "case has empty CaseID")
			continue
		}
		if c.RepositoryName == "" {
			validationErrors = append(validationErrors, fmt.Sprintf("case %s has empty RepositoryName", c.CaseID))
			continue
		}
		if len(c.RelevantFiles) == 0 {
			validationErrors = append(validationErrors, fmt.Sprintf("case %s has no relevant files specified", c.CaseID))
		}

		fixtureDir := GetFixturePathForRepo(c.RepositoryName)
		fixtureInfo, err := os.Stat(fixtureDir)
		if err != nil || !fixtureInfo.IsDir() {
			validationErrors = append(validationErrors, fmt.Sprintf("case %s repository fixture %q does not exist: %v", c.CaseID, fixtureDir, err))
			continue
		}

		// Verify directory is not empty
		entries, err := os.ReadDir(fixtureDir)
		if err != nil || len(entries) == 0 {
			validationErrors = append(validationErrors, fmt.Sprintf("case %s repository fixture %q is empty", c.CaseID, fixtureDir))
			continue
		}

		// Verify every relevant file actually exists in the target fixture repository
		for _, relFile := range c.RelevantFiles {
			if strings.Contains(relFile, "..") {
				validationErrors = append(validationErrors, fmt.Sprintf("case %s has invalid path traversal in relevant file %q", c.CaseID, relFile))
				continue
			}
			filePath := filepath.Join(fixtureDir, relFile)
			fileInfo, err := os.Stat(filePath)
			if err != nil {
				validationErrors = append(validationErrors, fmt.Sprintf("case %s (%s) references file %q which does not exist in %q",
					c.CaseID, c.IssueTitle, relFile, fixtureDir))
			} else if fileInfo.IsDir() || fileInfo.Size() == 0 {
				validationErrors = append(validationErrors, fmt.Sprintf("case %s file %q is empty or a directory", c.CaseID, relFile))
			}
		}

		// Verify relevant line ranges if present against actual file total line counts
		for relFile, lr := range c.RelevantLineRanges {
			if relFile == "" || lr.Start <= 0 || lr.End < lr.Start {
				validationErrors = append(validationErrors, fmt.Sprintf("case %s has invalid line range %s:%d-%d (must satisfy 1 <= start <= end)", c.CaseID, relFile, lr.Start, lr.End))
				continue
			}
			filePath := filepath.Join(fixtureDir, relFile)
			totalLines, err := countFileLines(filePath)
			if err != nil {
				validationErrors = append(validationErrors, fmt.Sprintf("case %s cannot read file %q to verify line count: %v", c.CaseID, relFile, err))
			} else if totalLines == 0 {
				validationErrors = append(validationErrors, fmt.Sprintf("case %s file %q has 0 lines", c.CaseID, relFile))
			} else if lr.End > totalLines {
				validationErrors = append(validationErrors, fmt.Sprintf("case %s line range %s:%d-%d exceeds actual file total lines (%d)", c.CaseID, relFile, lr.Start, lr.End, totalLines))
			}
		}

		// Ensure no empty title or description
		if strings.TrimSpace(c.IssueTitle) == "" || strings.TrimSpace(c.IssueDescription) == "" {
			validationErrors = append(validationErrors, fmt.Sprintf("case %s has empty issue title or description", c.CaseID))
		}
	}

	if len(validationErrors) > 0 {
		return fmt.Errorf("dataset ground truth validation failed with %d errors:\n- %s",
			len(validationErrors), strings.Join(validationErrors, "\n- "))
	}
	return nil
}

func countFileLines(filePath string) (int, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return 0, err
	}
	if len(data) == 0 {
		return 0, nil
	}
	lines := strings.Count(string(data), "\n")
	if !strings.HasSuffix(string(data), "\n") {
		lines++
	}
	return lines, nil
}
