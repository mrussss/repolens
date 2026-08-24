package llm

import (
	"context"
	"errors"
	"time"
)

var (
	ErrSimulatedRateLimit = errors.New("rate limit exceeded (429): retry after 2 seconds")
	ErrSimulatedServer500 = errors.New("provider server error (500): temporary failure")
)

type FakeProviderMode string

const (
	ModeNormalStructured FakeProviderMode = "NORMAL_STRUCTURED"
	ModeToolCallThenDone FakeProviderMode = "TOOL_CALL_THEN_DONE"
	ModeRateLimit429     FakeProviderMode = "RATE_LIMIT_429"
	ModeServer500        FakeProviderMode = "SERVER_500"
	ModeTimeout          FakeProviderMode = "TIMEOUT"
	ModeMalformedJSON    FakeProviderMode = "MALFORMED_JSON"
	ModeRepeatedToolCall FakeProviderMode = "REPEATED_TOOL_CALL"
)

type FakeProvider struct {
	Mode          FakeProviderMode
	CustomFinal   string
	SimulatedTool string
	ToolArgs      string
}

func NewFakeProvider(mode FakeProviderMode) *FakeProvider {
	return &FakeProvider{
		Mode:          mode,
		SimulatedTool: "search_code",
		ToolArgs:      `{"query":"nil pointer exception","top_k":3}`,
	}
}

func (f *FakeProvider) Generate(ctx context.Context, req GenerateRequest) (GenerateResponse, error) {
	switch f.Mode {
	case ModeRateLimit429:
		return GenerateResponse{}, ErrSimulatedRateLimit
	case ModeServer500:
		return GenerateResponse{}, ErrSimulatedServer500
	case ModeTimeout:
		select {
		case <-time.After(5 * time.Second):
			return GenerateResponse{}, context.DeadlineExceeded
		case <-ctx.Done():
			return GenerateResponse{}, ctx.Err()
		}
	case ModeMalformedJSON:
		return GenerateResponse{
			Message: Message{
				Role:    RoleAssistant,
				Content: `{ "summary": "incomplete json ...`,
			},
			FinishReason:     "stop",
			PromptTokens:     100,
			CompletionTokens: 50,
		}, nil
	case ModeRepeatedToolCall:
		var tc ToolCall
		tc.ID = "call_repeat_1"
		tc.Type = "function"
		tc.Function.Name = f.SimulatedTool
		tc.Function.Arguments = f.ToolArgs

		return GenerateResponse{
			Message: Message{
				Role:      RoleAssistant,
				ToolCalls: []ToolCall{tc},
			},
			FinishReason:     "tool_calls",
			PromptTokens:     80,
			CompletionTokens: 30,
		}, nil
	case ModeToolCallThenDone:
		// Check if last message is tool result
		hasToolResult := false
		for _, m := range req.Messages {
			if m.Role == RoleTool {
				hasToolResult = true
				break
			}
		}

		if !hasToolResult {
			// Step 1: Issue tool call
			var tc ToolCall
			tc.ID = "call_123"
			tc.Type = "function"
			tc.Function.Name = f.SimulatedTool
			tc.Function.Arguments = f.ToolArgs

			return GenerateResponse{
				Message: Message{
					Role:      RoleAssistant,
					ToolCalls: []ToolCall{tc},
				},
				FinishReason:     "tool_calls",
				PromptTokens:     120,
				CompletionTokens: 40,
			}, nil
		}

		// Step 2: Return final structured JSON
		return f.normalStructuredResponse()

	default: // ModeNormalStructured
		return f.normalStructuredResponse()
	}
}

func (f *FakeProvider) normalStructuredResponse() (GenerateResponse, error) {
	content := f.CustomFinal
	if content == "" {
		content = `{
  "summary": "Investigated error logs and located root cause in repository configuration and logic.",
  "root_cause": "Null pointer dereference or missing validation when initializing database connection and config",
  "findings": [
    {
      "title": "Unchecked pointer dereference in service initialization",
      "reasoning": "The application assumes configuration value or database connection pointer is non-empty without default checks.",
      "citations": [
        {
          "path": "internal/platform/config/config.go",
          "start_line": 1,
          "end_line": 10
        }
      ]
    }
  ],
  "recommended_checks": [
    "Verify DSN before invoking database connection",
    "Add regression test cases"
  ],
  "confidence": 0.92
}`
	}

	return GenerateResponse{
		Message: Message{
			Role:    RoleAssistant,
			Content: content,
		},
		FinishReason:     "stop",
		PromptTokens:     200,
		CompletionTokens: 250,
	}, nil
}
