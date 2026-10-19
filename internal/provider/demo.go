package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
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
	demoCommit = "f5e4d3c2b1a09876543210fedcba98765432100124"
)

var demoInitializationMu sync.Mutex

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
	// 本地单进程 API 串行准备共享 fixture；已经 READY 的源码只读复用。
	demoInitializationMu.Lock()
	defer demoInitializationMu.Unlock()
	var result gin.H
	err := h.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 普通 Store 会附带创建待执行 Job。让这些 Job 的创建和精确清理
		// 留在同一事务中，Worker 就不会与同步 Demo 构建争夺执行权。
		scoped := &Handler{
			db: tx, repoStore: repo.NewStore(tx), snapshotStore: snapshot.NewStore(tx),
			codeIntelStore: codeintelstore.NewStore(tx), reportStore: evidence.NewReportStore(tx),
			citationStore: evidence.NewCitationStore(tx), traceStore: trace.NewStore(tx),
			storeFS: h.storeFS, indexStorageDir: h.indexStorageDir,
		}
		var err error
		result, err = scoped.initializeRealDemo(ctx, userID)
		return err
	})
	if err != nil {
		writeProviderInternalError(c, "DEMO_INITIALIZATION_FAILED", "failed to initialize demo", err)
		return
	}
	c.JSON(http.StatusCreated, result)
}

func (h *Handler) initializeRealDemo(ctx context.Context, userID string) (gin.H, error) {
	demoDir := h.storeFS.GetSourcePath(demoRepoID, demoSnapID)
	repository, err := h.repoStore.GetByID(ctx, demoRepoID)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	if repository == nil {
		repository = &repo.Repository{ID: demoRepoID, UserID: userID, Name: "order-service（Demo）", GitURL: "https://github.com/repolens/demo-order-service", DefaultRef: "main", Status: "ACTIVE"}
		if err := h.repoStore.Create(ctx, repository); err != nil {
			return nil, err
		}
	}
	if repository.UserID != userID {
		repository.UserID = userID
		if err := h.repoStore.Update(ctx, repository); err != nil {
			return nil, err
		}
	}

	now := time.Now().UTC()
	demoSnapshot, err := h.snapshotStore.GetByID(ctx, demoSnapID)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	if demoSnapshot == nil {
		if err := makeDemoSource(demoDir); err != nil {
			return nil, err
		}
		contentHash, fileCount, totalBytes, err := hashDemoSource(demoDir)
		if err != nil {
			return nil, err
		}
		demoSnapshot = &snapshot.RepositorySnapshot{ID: demoSnapID, RepositoryID: demoRepoID, CommitSHA: demoCommit, Ref: "main", MaterializedPath: demoDir, Status: snapshot.StatusReady, ContentHash: contentHash, FileCount: fileCount, TotalBytes: totalBytes, ReadyAt: &now}
		if err := h.snapshotStore.Create(ctx, demoSnapshot); err != nil {
			return nil, err
		}
	} else if demoSnapshot.Status != snapshot.StatusReady || demoSnapshot.RepositoryID != demoRepoID || demoSnapshot.CommitSHA != demoCommit {
		return nil, errors.New("existing demo snapshot identity is inconsistent")
	} else {
		demoDir = demoSnapshot.MaterializedPath
	}

	build, _, err := h.codeIntelStore.GetOrCreateBuild(ctx, demoSnapID, "github.com/repolens/demo-order-service/orders", codeintelmodel.DefaultBuildContext())
	if err != nil {
		return nil, err
	}
	if build.Status == codeintelmodel.BuildStatusReady && build.SymbolCount == 0 {
		if err := resetDemoCodeBuild(h.db, build.ID); err != nil {
			return nil, err
		}
		build.Status = codeintelmodel.BuildStatusBuilding
	}
	if build.Status != codeintelmodel.BuildStatusReady {
		if err := h.codeIntelStore.MarkBuildBuilding(ctx, build.ID); err != nil {
			return nil, err
		}
		analysis, analyzeErr := codeintel.NewAnalyzer().Analyze(ctx, demoDir, codeintelmodel.DefaultBuildContext())
		if analyzeErr != nil {
			return nil, analyzeErr
		}
		if err := h.codeIntelStore.SaveAnalysisResult(ctx, build.ID, analysis); err != nil {
			return nil, err
		}
	}
	build, err = h.codeIntelStore.GetByID(ctx, build.ID)
	if err != nil {
		return nil, err
	}

	retrievalBuild, _, err := h.codeIntelStore.GetOrCreateRetrievalBuild(ctx, build.ID, codeintelmodel.ProductionRetrievalStrategy)
	if err != nil {
		return nil, err
	}
	if retrievalBuild.Status == codeintelmodel.BuildStatusReady && retrievalBuild.DocumentCount == 0 {
		if err := resetDemoRetrievalBuild(h.db, retrievalBuild.ID); err != nil {
			return nil, err
		}
		retrievalBuild.Status = codeintelmodel.BuildStatusBuilding
	}
	if retrievalBuild.Status != codeintelmodel.BuildStatusReady {
		if err := h.codeIntelStore.MarkRetrievalBuilding(ctx, retrievalBuild.ID); err != nil {
			return nil, err
		}
		if err := publishDemoRetrieval(ctx, h.codeIntelStore, build.ID, retrievalBuild, h.indexStorageDir); err != nil {
			return nil, err
		}
	}
	// Demo artifacts are prebuilt and therefore must not leave ordinary pending jobs.
	// 两类 Build 的数字 ID 来自不同表，必须按 (job_type, resource_id)
	// 精确配对，避免删掉恰好同号的其他仓库任务。
	if err := h.db.Where("(job_type = ? AND resource_id = ?) OR (job_type = ? AND resource_id = ?)",
		jobs.JobTypeBuildCodeIndex, fmt.Sprint(build.ID), jobs.JobTypeBuildRetrieval, fmt.Sprint(retrievalBuild.ID)).Delete(&jobs.AnalysisJob{}).Error; err != nil {
		return nil, err
	}
	// Remove legacy v1 demo diagnosis jobs, if a database was upgraded from
	// the old demo implementation that created a normal pending job.
	if err := h.db.Where("job_type = ? AND resource_id LIKE ?", jobs.JobTypeRunDiagnosis, "diag-demo-%").Delete(&jobs.AnalysisJob{}).Error; err != nil {
		return nil, err
	}

	runID := "diag-demo-" + uuid.New().String()[:8]
	issueTitle := "[Demo] 高并发下订单提交 Worker 死锁"
	demoRun := &diagnosis.DiagnosisRun{ID: runID, UserID: userID, RepositoryID: demoRepoID, SnapshotID: demoSnapID, CodeIndexBuildID: build.ID, RetrievalBuildID: retrievalBuild.ID, IssueTitle: issueTitle, IssueDescription: "高并发订单负载下 HTTP 处理器挂起，超过 100 个请求后不再接受新订单。", ErrorLog: "panic: deadlock detected in goroutine 42 [chan send]: orders.(*OrderProcessor).SubmitOrder processor.go:14", Status: diagnosis.StatusSucceeded, IdempotencyKey: "idemp-demo-" + uuid.New().String()[:8], IdempotencyRequestHash: "hash-demo", Version: 1, CreatedAt: now, UpdatedAt: now}
	if err := h.db.Create(demoRun).Error; err != nil {
		return nil, err
	}
	attemptID := "attempt-demo-" + uuid.New().String()[:8]
	attempt := &diagnosis.DiagnosisAttempt{ID: attemptID, DiagnosisRunID: runID, AttemptNo: 1, WorkerID: "demo", Status: diagnosis.AttemptStatusSucceeded, StartedAt: now.Add(-2 * time.Second), HeartbeatAt: now, DeadlineAt: now.Add(time.Hour), FinishedAt: &now, PromptTokens: 430, CompletionTokens: 380, ToolCalls: 2}
	if err := h.db.Create(attempt).Error; err != nil {
		return nil, err
	}
	if err := h.db.Model(&diagnosis.DiagnosisRun{}).Where("id = ?", runID).Update("final_attempt_id", attemptID).Error; err != nil {
		return nil, err
	}

	reportID := "report-demo-" + uuid.New().String()[:8]
	excerpt, err := h.storeFS.ReadFile(ctx, demoRepoID, demoSnapID, "processor.go", 9, 16)
	if err != nil {
		return nil, err
	}
	citation := evidence.Citation{ID: uuid.New().String(), ReportID: reportID, SnapshotID: demoSnapID, CodeIndexBuildID: build.ID, FilePath: "processor.go", StartLine: 9, EndLine: 16, Excerpt: excerpt, Reason: "无缓冲 channel 的构造与阻塞发送", CreatedAt: now}
	evidence.NewCitationValidator(h.storeFS).Validate(ctx, demoRepoID, demoSnapID, &citation)
	data := &evidence.DiagnosisReportData{
		ConclusionKind:    evidence.ConclusionRootCause,
		Summary:           "订单提交向无缓冲 channel 发送数据，缺少消费者时请求会一直阻塞。",
		RootCause:         "无缓冲 jobsChan 将订单提交变成同步阻塞操作；没有消费者时所有 HTTP 请求都会卡在发送处。",
		Findings:          []evidence.Finding{{Title: "无缓冲 channel 导致订单提交阻塞", Reasoning: "NewOrderProcessor 使用无缓冲 jobsChan，SubmitOrder 的发送必须等待接收者；缺少消费者时发送方会一直阻塞。", Citations: []evidence.Citation{citation}}},
		RecommendedChecks: []string{"为 jobsChan 增加容量并评估背压策略", "在 SubmitOrder 中使用 ctx.Done() 或超时兜底", "增加高并发 channel 排空集成测试"},
		Limitations:       []string{"这是预置 Demo，报告和 Trace 用于演示界面及源码引用校验，未调用真实 Provider。"},
	}
	quality, err := evidence.ClassifyReport(data, true)
	if err != nil || quality.Status != evidence.ReportValid {
		return nil, fmt.Errorf("demo report status %s: %v", quality.Status, err)
	}
	findingsJSON, _ := json.Marshal(data.Findings)
	checksJSON, _ := json.Marshal(data.RecommendedChecks)
	limitationsJSON, _ := json.Marshal(data.Limitations)
	payloadJSON, _ := json.Marshal(data)
	report := &evidence.Report{
		ID: reportID, DiagnosisRunID: runID, AttemptID: attemptID,
		ConclusionKind: data.ConclusionKind, ReportStatus: quality.Status, Summary: data.Summary, RootCause: data.RootCause,
		FindingsJSON: string(findingsJSON), RecommendedChecksJSON: string(checksJSON), StructuredPayloadJSON: string(payloadJSON), LimitationsJSON: string(limitationsJSON),
		FindingCount: quality.FindingCount, SupportedFindingCount: quality.SupportedFindingCount, UnsupportedFindingCount: quality.UnsupportedFindingCount,
		ValidCitationCount: quality.ValidCitationCount, InvalidCitationCount: quality.InvalidCitationCount, CitationCoverage: quality.CitationCoverage,
		CreatedAt: now,
	}
	if err := h.reportStore.Create(ctx, report); err != nil {
		return nil, err
	}
	if h.citationStore != nil {
		if err := h.citationStore.CreateBatch(ctx, []evidence.Citation{citation}); err != nil {
			return nil, err
		}
	}
	if err := h.traceStore.Create(ctx, &trace.AgentStep{ID: uuid.New().String(), AttemptID: attemptID, Seq: 1, StepType: trace.StepTypeToolCall, ToolName: "search_code", ToolArgsSummary: `{"query":"OrderProcessor SubmitOrder jobsChan"}`, ToolResultSummary: "processor.go:L9-L16", Status: "COMPLETED", LatencyMs: 45, InputTokens: 250, OutputTokens: 60, CreatedAt: now.Add(-2 * time.Second)}); err != nil {
		return nil, err
	}
	if err := h.traceStore.Create(ctx, &trace.AgentStep{ID: uuid.New().String(), AttemptID: attemptID, Seq: 2, StepType: trace.StepTypeToolCall, ToolName: "read_file", ToolArgsSummary: `{"path":"processor.go"}`, ToolResultSummary: "processor.go:L9-L16", Status: "COMPLETED", LatencyMs: 12, InputTokens: 180, OutputTokens: 320, CreatedAt: now.Add(-time.Second)}); err != nil {
		return nil, err
	}

	return gin.H{"message": "Demo 环境与诊断已初始化", "diagnosis_id": runID, "repository_id": demoRepoID, "snapshot_id": demoSnapID, "report": report}, nil
}

func makeDemoSource(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return nil
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

func resetDemoCodeBuild(db *gorm.DB, buildID int64) error {
	for _, model := range []interface{}{&codeintelmodel.SymbolRelation{}, &codeintelmodel.Symbol{}, &codeintelmodel.CodeFile{}} {
		if err := db.Where("code_index_build_id = ?", buildID).Delete(model).Error; err != nil {
			return err
		}
	}
	return db.Model(&codeintelmodel.CodeIndexBuild{}).Where("id = ?", buildID).Updates(map[string]interface{}{"status": codeintelmodel.BuildStatusBuilding, "symbol_count": 0}).Error
}
func resetDemoRetrievalBuild(db *gorm.DB, buildID int64) error {
	return db.Model(&codeintelmodel.RetrievalBuild{}).Where("id = ?", buildID).Updates(map[string]interface{}{"status": codeintelmodel.BuildStatusBuilding, "document_count": 0, "artifact_path": "", "artifact_hash": ""}).Error
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
	path, hash, err := artifact.NewPublisher(baseDir).Publish(rb.ID, 1, "demo-"+uuid.NewString(), rb.Strategy, idx)
	if err != nil {
		return err
	}
	return store.CompleteRetrievalBuild(ctx, rb.ID, path, hash, idx.TotalDocs)
}
