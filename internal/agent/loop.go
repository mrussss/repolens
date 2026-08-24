package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/llm"
	"repolens/internal/platform/logger"
	"repolens/internal/sse"
	"repolens/internal/trace"
)

const SystemPrompt = `You are RepoLens, an expert AI repository diagnosis engineer.
Your task is to analyze code repositories and error logs, determine root causes, and provide evidence-backed reports.

Rules:
1. Always base your findings on actual source code retrieved through tools.
2. Use tools to search and read code before drawing conclusions.
3. Your final response MUST be a valid JSON object matching this schema:
{
  "summary": "High-level summary of the issue",
  "root_cause": "Detailed explanation of the root cause",
  "findings": [
    {
      "title": "Finding title",
      "reasoning": "Technical reasoning",
      "citations": [
        {
          "path": "path/to/file.ext",
          "start_line": 10,
          "end_line": 25,
          "excerpt": "relevant snippet",
          "reason": "why this line is relevant"
        }
      ]
    }
  ],
  "recommended_checks": [
    "Actionable fix step 1",
    "Actionable fix step 2"
  ],
  "confidence": 0.95
}
Do not wrap the JSON with markdown backticks if possible, or output strictly parseable JSON.`

type LoopResult struct {
	Report           *evidence.DiagnosisReportData
	RawOutput        string
	PromptTokens     int
	CompletionTokens int
	ToolCallsCount   int
}

type AgentLoop struct {
	provider   llm.Provider
	registry   *ToolRegistry
	traceStore trace.Store
	sseHub     *sse.Hub
	guardCfg   GuardConfig
}

func NewAgentLoop(
	provider llm.Provider,
	registry *ToolRegistry,
	traceStore trace.Store,
	sseHub *sse.Hub,
	guardCfg GuardConfig,
) *AgentLoop {
	return &AgentLoop{
		provider:   provider,
		registry:   registry,
		traceStore: traceStore,
		sseHub:     sseHub,
		guardCfg:   guardCfg,
	}
}

func (l *AgentLoop) Run(ctx context.Context, run *diagnosis.DiagnosisRun, attempt *diagnosis.DiagnosisAttempt) (*LoopResult, error) {
	guard := NewAgentGuard(l.guardCfg)
	toolsDef := l.registry.Definitions()

	initialUserMsg := fmt.Sprintf("Repository ID: %s\nSnapshot ID: %s\nIssue Title: %s\n\nIssue Description:\n%s\n\nError Log / CI Log:\n%s",
		run.RepositoryID,
		run.SnapshotID,
		run.IssueTitle,
		run.IssueDescription,
		run.ErrorLog,
	)

	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: SystemPrompt},
		{Role: llm.RoleUser, Content: initialUserMsg},
	}

	totalPromptTokens := 0
	totalCompletionTokens := 0
	toolCallsCount := 0
	seq := 0

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		seq++
		if err := guard.RecordStep(); err != nil {
			return nil, fmt.Errorf("guard limit reached: %w", err)
		}

		startGen := time.Now()
		resp, err := l.provider.Generate(ctx, llm.GenerateRequest{
			Messages:    messages,
			Tools:       toolsDef,
			Temperature: 0.1,
		})
		latency := time.Since(startGen).Milliseconds()

		if err != nil {
			// Record error step
			_ = l.recordStep(ctx, attempt.ID, seq, trace.StepTypeError, "", "", "", "FAILED", latency, 0, 0, "LLM_ERROR: "+err.Error())
			return nil, err
		}

		totalPromptTokens += resp.PromptTokens
		totalCompletionTokens += resp.CompletionTokens

		// Check if assistant called tools
		if len(resp.Message.ToolCalls) > 0 {
			messages = append(messages, resp.Message)

			for _, tc := range resp.Message.ToolCalls {
				toolCallsCount++
				if err := guard.RecordToolCall(tc.Function.Name, tc.Function.Arguments); err != nil {
					return nil, err
				}

				// Record tool call step
				_ = l.recordStep(ctx, attempt.ID, seq, trace.StepTypeToolCall, tc.Function.Name, tc.Function.Arguments, "", "COMPLETED", latency, resp.PromptTokens, resp.CompletionTokens, "")
				l.publishSSE(run.ID, "tool.called", map[string]interface{}{
					"tool_name": tc.Function.Name,
					"arguments": tc.Function.Arguments,
				})

				// Execute tool
				t, err := l.registry.Get(tc.Function.Name)
				var toolResult string
				if err != nil {
					toolResult = fmt.Sprintf("Error: %v", err)
				} else {
					toolExecStart := time.Now()
					toolResult, err = t.Execute(ctx, tc.Function.Arguments)
					toolExecLatency := time.Since(toolExecStart).Milliseconds()
					if err != nil {
						toolResult = fmt.Sprintf("Tool Execution Error: %v", err)
					}

					seq++
					_ = l.recordStep(ctx, attempt.ID, seq, trace.StepTypeToolResult, tc.Function.Name, "", toolResult, "COMPLETED", toolExecLatency, 0, 0, "")
					l.publishSSE(run.ID, "tool.completed", map[string]interface{}{
						"tool_name":  tc.Function.Name,
						"result_len": len(toolResult),
					})
				}

				messages = append(messages, llm.Message{
					Role:       llm.RoleTool,
					ToolCallID: tc.ID,
					Content:    toolResult,
				})
			}
			continue
		}

		// Final response received
		finalText := resp.Message.Content
		_ = l.recordStep(ctx, attempt.ID, seq, trace.StepTypeFinalOutput, "", "", finalText, "COMPLETED", latency, resp.PromptTokens, resp.CompletionTokens, "")

		reportData, err := parseReportJSON(finalText)
		if err != nil {
			logger.L(ctx).Warn("failed to parse structured report JSON from assistant output", "error", err, "raw", finalText)
			// Return minimal structured output with raw content as root cause
			reportData = &evidence.DiagnosisReportData{
				Summary:    run.IssueTitle,
				RootCause:  finalText,
				Confidence: 0.7,
			}
		}

		return &LoopResult{
			Report:           reportData,
			RawOutput:        finalText,
			PromptTokens:     totalPromptTokens,
			CompletionTokens: totalCompletionTokens,
			ToolCallsCount:   toolCallsCount,
		}, nil
	}
}

func (l *AgentLoop) recordStep(ctx context.Context, attemptID string, seq int, stepType trace.StepType, toolName, args, result, status string, latency int64, inTok, outTok int, errCode string) error {
	if l.traceStore == nil {
		return nil
	}
	step := &trace.AgentStep{
		ID:                uuid.New().String(),
		AttemptID:         attemptID,
		Seq:               seq,
		StepType:          stepType,
		ToolName:          toolName,
		ToolArgsSummary:   args,
		ToolResultSummary: result,
		Status:            status,
		LatencyMs:         latency,
		InputTokens:       inTok,
		OutputTokens:      outTok,
		ErrorCode:         errCode,
		CreatedAt:         time.Now(),
	}
	return l.traceStore.Create(ctx, step)
}

func (l *AgentLoop) publishSSE(runID, eventType string, data interface{}) {
	if l.sseHub != nil {
		l.sseHub.Publish(runID, sse.Event{
			Type:      eventType,
			Timestamp: time.Now(),
			Data:      data,
		})
	}
}

var jsonExtractorRegex = regexp.MustCompile(`(?s)\{.*\}`)

func parseReportJSON(raw string) (*evidence.DiagnosisReportData, error) {
	clean := strings.TrimSpace(raw)
	// Strip markdown code block wrappers if any
	clean = strings.TrimPrefix(clean, "```json")
	clean = strings.TrimPrefix(clean, "```")
	clean = strings.TrimSuffix(clean, "```")
	clean = strings.TrimSpace(clean)

	var report evidence.DiagnosisReportData
	if err := json.Unmarshal([]byte(clean), &report); err == nil {
		if report.RootCause != "" {
			return &report, nil
		}
	}

	// Try extracting JSON block via regex
	m := jsonExtractorRegex.FindString(raw)
	if m != "" {
		if err := json.Unmarshal([]byte(m), &report); err == nil {
			if report.RootCause != "" {
				return &report, nil
			}
		}
	}

	return nil, errors.New("cannot parse valid structured report JSON from LLM output")
}
