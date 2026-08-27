package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"repolens/internal/codeintel"
	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/jobs"
	"repolens/internal/repo"
	"repolens/internal/retrieval/artifact"
	"repolens/internal/retrieval/bm25"
	"repolens/internal/snapshot"
	"repolens/internal/trace"
)

const (
	demoRepoID = "repo-demo-order-svc"
	demoSnapID = "snap-demo-order-svc-002"
	demoCommit = "e4d3c2b1a09876543210fedcba98765432100123"
)

const demoModule = `module github.com/repolens/demo-order-service

go 1.22
`

const demoProcessor = `package orders

import "context"

type Order struct { ID string; Amount float64 }

type OrderProcessor struct { jobsChan chan Order }

func NewOrderProcessor() *OrderProcessor {
	return &OrderProcessor{jobsChan: make(chan Order)}
}

func (p *OrderProcessor) SubmitOrder(ctx context.Context, order Order) error {
	p.jobsChan <- order
	return nil
}
`

const demoProcessorTest = `package orders

import "testing"

func TestSubmitOrder(t *testing.T) {
	p := NewOrderProcessor()
	if p == nil { t.Fatal("processor is nil") }
}
`

func (h *Handler) triggerRealDemo(c *gin.Context) {
	ctx := c.Request.Context()
	userID := c.GetString("user_id")
	if userID == "" {
		userID = "local-user"
	}
	demoDir := h.storeFS.GetSourcePath(demoRepoID, demoSnapID)
	if err := makeDemoSource(demoDir); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	repository, _ := h.repoStore.GetByID(ctx, demoRepoID)
	if repository == nil {
		repository = &repo.Repository{ID: demoRepoID, UserID: userID, Name: "order-service（Demo）", GitURL: "https://github.com/repolens/demo-order-service", DefaultRef: "main", Status: "ACTIVE"}
		if err := h.repoStore.Create(ctx, repository); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
	if repository.UserID != userID {
		repository.UserID = userID
		_ = h.repoStore.Update(ctx, repository)
	}

	now := time.Now().UTC()
	contentHash, fileCount, totalBytes, err := hashDemoSource(demoDir)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	demoSnapshot, _ := h.snapshotStore.GetByID(ctx, demoSnapID)
	if demoSnapshot == nil {
		demoSnapshot = &snapshot.RepositorySnapshot{ID: demoSnapID, RepositoryID: demoRepoID, CommitSHA: demoCommit, Ref: "main", MaterializedPath: demoDir, Status: snapshot.StatusReady, ContentHash: contentHash, FileCount: fileCount, TotalBytes: totalBytes, ReadyAt: &now}
		if err := h.snapshotStore.Create(ctx, demoSnapshot); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	} else {
		h.db.Model(&snapshot.RepositorySnapshot{}).Where("id = ?", demoSnapID).Updates(map[string]interface{}{"status": snapshot.StatusReady, "commit_sha": demoCommit, "content_hash": contentHash, "file_count": fileCount, "total_bytes": totalBytes, "ready_at": now})
	}

	build, _, err := h.codeIntelStore.GetOrCreateBuild(ctx, demoSnapID, "github.com/repolens/demo-order-service/orders", codeintelmodel.DefaultBuildContext())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if build.Status == codeintelmodel.BuildStatusReady && build.SymbolCount == 0 {
		resetDemoCodeBuild(h.db, build.ID)
	}
	build, _ = h.codeIntelStore.GetByID(ctx, build.ID)
	if build.Status != codeintelmodel.BuildStatusReady {
		if err := h.codeIntelStore.MarkBuildBuilding(ctx, build.ID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		analysis, analyzeErr := codeintel.NewAnalyzer().Analyze(ctx, demoDir, codeintelmodel.DefaultBuildContext())
		if analyzeErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": analyzeErr.Error()})
			return
		}
		if err := h.codeIntelStore.SaveAnalysisResult(ctx, build.ID, analysis); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
	build, _ = h.codeIntelStore.GetByID(ctx, build.ID)

	retrievalBuild, _, err := h.codeIntelStore.GetOrCreateRetrievalBuild(ctx, build.ID, "symbol_bm25_structural")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if retrievalBuild.Status == codeintelmodel.BuildStatusReady && retrievalBuild.DocumentCount == 0 {
		resetDemoRetrievalBuild(h.db, retrievalBuild.ID)
	}
	retrievalBuild, _ = h.codeIntelStore.GetRetrievalBuildByID(ctx, retrievalBuild.ID)
	if retrievalBuild.Status != codeintelmodel.BuildStatusReady {
		if err := h.codeIntelStore.MarkRetrievalBuilding(ctx, retrievalBuild.ID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if err := publishDemoRetrieval(ctx, h.codeIntelStore, build.ID, retrievalBuild, h.indexStorageDir); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
	// Demo artifacts are prebuilt and therefore must not leave ordinary pending jobs.
	h.db.Where("job_type IN ? AND resource_id IN ?", []jobs.JobType{jobs.JobTypeBuildCodeIndex, jobs.JobTypeBuildRetrieval}, []string{fmt.Sprint(build.ID), fmt.Sprint(retrievalBuild.ID)}).Delete(&jobs.AnalysisJob{})
	// Remove legacy v1 demo diagnosis jobs, if a database was upgraded from
	// the old demo implementation that created a normal pending job.
	h.db.Where("job_type = ? AND resource_id LIKE ?", jobs.JobTypeRunDiagnosis, "diag-demo-%").Delete(&jobs.AnalysisJob{})

	runID := "diag-demo-" + uuid.New().String()[:8]
	issueTitle := "[Demo] 高并发下订单提交 Worker 死锁"
	demoRun := &diagnosis.DiagnosisRun{ID: runID, UserID: userID, RepositoryID: demoRepoID, SnapshotID: demoSnapID, CodeIndexBuildID: build.ID, RetrievalBuildID: retrievalBuild.ID, IssueTitle: issueTitle, IssueDescription: "高并发订单负载下 HTTP 处理器挂起，超过 100 个请求后不再接受新订单。", ErrorLog: "panic: deadlock detected in goroutine 42 [chan send]: orders.(*OrderProcessor).SubmitOrder processor.go:14", Status: diagnosis.StatusSucceeded, IdempotencyKey: "idemp-demo-" + uuid.New().String()[:8], IdempotencyRequestHash: "hash-demo", Version: 1, CreatedAt: now, UpdatedAt: now}
	if err := h.db.Create(demoRun).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	attemptID := "attempt-demo-" + uuid.New().String()[:8]
	attempt := &diagnosis.DiagnosisAttempt{ID: attemptID, DiagnosisRunID: runID, AttemptNo: 1, WorkerID: "demo", Status: diagnosis.AttemptStatusSucceeded, StartedAt: now.Add(-2 * time.Second), HeartbeatAt: now, DeadlineAt: now.Add(time.Hour), FinishedAt: &now, PromptTokens: 430, CompletionTokens: 380, ToolCalls: 2}
	if err := h.db.Create(attempt).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.db.Model(&diagnosis.DiagnosisRun{}).Where("id = ?", runID).Updates(map[string]interface{}{"final_attempt_id": attemptID})

	findings := []map[string]interface{}{{"title": "无缓冲 channel 导致订单提交阻塞", "reasoning": "NewOrderProcessor 使用无缓冲 jobsChan，SubmitOrder 的发送必须等待接收者；并发负载下发送方会永久阻塞，最终形成 Goroutine 死锁。", "citations": []map[string]interface{}{{"snapshot_id": demoSnapID, "file_path": "processor.go", "start_line": 10, "end_line": 16, "excerpt": "func (p *OrderProcessor) SubmitOrder(ctx context.Context, order Order) error {\n\tp.jobsChan <- order\n\treturn nil\n}", "reason": "无缓冲 channel 的阻塞发送"}}}}
	findingsJSON, _ := json.Marshal(findings)
	checksJSON, _ := json.Marshal([]string{"为 jobsChan 增加容量并评估背压策略", "在 SubmitOrder 中使用 ctx.Done() 或超时兜底", "增加高并发 channel 排空集成测试"})
	report := &evidence.Report{ID: "report-demo-" + uuid.New().String()[:8], DiagnosisRunID: runID, AttemptID: attemptID, RootCause: "无缓冲 jobsChan 将订单提交变成同步阻塞操作；没有消费者时所有 HTTP 请求都会卡在发送处。", FindingsJSON: string(findingsJSON), RecommendedChecksJSON: string(checksJSON), Confidence: 0.98, CreatedAt: now}
	if err := h.reportStore.Create(ctx, report); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	citation := evidence.Citation{ID: uuid.New().String(), ReportID: report.ID, SnapshotID: demoSnapID, CodeIndexBuildID: build.ID, FilePath: "processor.go", StartLine: 10, EndLine: 16, Reason: "无缓冲 channel 的阻塞发送", CreatedAt: now}
	evidence.NewCitationValidator(h.storeFS).Validate(ctx, demoRepoID, demoSnapID, &citation)
	if h.citationStore != nil {
		_ = h.citationStore.CreateBatch(ctx, []evidence.Citation{citation})
	}
	_ = h.traceStore.Create(ctx, &trace.AgentStep{ID: uuid.New().String(), AttemptID: attemptID, Seq: 1, StepType: trace.StepTypeToolCall, ToolName: "search_code", ToolArgsSummary: `{"query":"OrderProcessor SubmitOrder jobsChan"}`, ToolResultSummary: "processor.go:L10-L16", Status: "COMPLETED", LatencyMs: 45, InputTokens: 250, OutputTokens: 60, CreatedAt: now.Add(-2 * time.Second)})
	_ = h.traceStore.Create(ctx, &trace.AgentStep{ID: uuid.New().String(), AttemptID: attemptID, Seq: 2, StepType: trace.StepTypeToolCall, ToolName: "read_file", ToolArgsSummary: `{"path":"processor.go"}`, ToolResultSummary: "processor.go:18 lines", Status: "COMPLETED", LatencyMs: 12, InputTokens: 180, OutputTokens: 320, CreatedAt: now.Add(-time.Second)})

	c.JSON(http.StatusCreated, gin.H{"message": "Demo 环境与诊断已初始化", "diagnosis_id": runID, "repository_id": demoRepoID, "snapshot_id": demoSnapID, "report": report})
}

func makeDemoSource(dir string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return os.Chmod(path, 0755)
		}
		return os.Chmod(path, 0644)
	})
}

func hashDemoSource(dir string) (string, int, int64, error) {
	files := map[string][]byte{"go.mod": []byte(demoModule), "processor.go": []byte(demoProcessor), "processor_test.go": []byte(demoProcessorTest)}
	h := sha256.New()
	var total int64
	for _, name := range []string{"go.mod", "processor.go", "processor_test.go"} {
		data := files[name]
		if err := os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
			return "", 0, 0, err
		}
		h.Write([]byte(name))
		h.Write(data)
		total += int64(len(data))
	}
	return hex.EncodeToString(h.Sum(nil)), len(files), total, nil
}

func resetDemoCodeBuild(db *gorm.DB, buildID int64) {
	db.Where("code_index_build_id = ?", buildID).Delete(&codeintelmodel.SymbolRelation{})
	db.Where("code_index_build_id = ?", buildID).Delete(&codeintelmodel.Symbol{})
	db.Where("code_index_build_id = ?", buildID).Delete(&codeintelmodel.CodeFile{})
	db.Model(&codeintelmodel.CodeIndexBuild{}).Where("id = ?", buildID).Updates(map[string]interface{}{"status": codeintelmodel.BuildStatusBuilding, "symbol_count": 0})
}
func resetDemoRetrievalBuild(db *gorm.DB, buildID int64) {
	db.Model(&codeintelmodel.RetrievalBuild{}).Where("id = ?", buildID).Updates(map[string]interface{}{"status": codeintelmodel.BuildStatusBuilding, "document_count": 0, "artifact_path": "", "artifact_hash": ""})
}

func publishDemoRetrieval(ctx context.Context, store codeintelstore.Store, buildID int64, rb *codeintelmodel.RetrievalBuild, baseDir string) error {
	symbols, err := store.ListSymbols(ctx, buildID, "", 10000)
	if err != nil {
		return err
	}
	idx := bm25.NewIndex(1.2, 0.75)
	for _, sym := range symbols {
		idx.AddDocument(bm25.Document{FilePath: sym.FilePath, StartLine: sym.StartLine, EndLine: sym.EndLine, Content: sym.Signature + " " + sym.Doc + " " + sym.Name, SymbolKeyHash: sym.SymbolKeyHash, SymbolName: sym.Name, Kind: string(sym.Kind)})
	}
	idx.Build()
	path, hash, err := artifact.NewPublisher(baseDir).Publish(rb.ID, "demo", rb.Strategy, idx)
	if err != nil {
		return err
	}
	return store.CompleteRetrievalBuild(ctx, rb.ID, path, hash, idx.TotalDocs)
}
