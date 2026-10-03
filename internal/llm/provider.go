package llm

import (
	"context"
	"errors"
)

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type Message struct {
	Role             Role       `json:"role"`
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

type ToolFunction struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

type FunctionDef = ToolFunction

type ToolDefinition struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type GenerateRequest struct {
	Model           string           `json:"model"`
	Messages        []Message        `json:"messages"`
	Tools           []ToolDefinition `json:"tools,omitempty"`
	Temperature     *float64         `json:"temperature,omitempty"`
	MaxTokens       int              `json:"max_tokens,omitempty"`
	ReasoningEffort string           `json:"reasoning_effort,omitempty"`
	ResponseFormat  *ResponseFormat  `json:"response_format,omitempty"`
}

type ResponseFormat struct {
	Type string `json:"type"`
}

type GenerateResponse struct {
	Message            Message `json:"message"`
	FinishReason       string  `json:"finish_reason"`
	PromptTokens       int     `json:"prompt_tokens"`
	CompletionTokens   int     `json:"completion_tokens"`
	CachedPromptTokens int     `json:"cached_prompt_tokens"`
	ReasoningTokens    int     `json:"reasoning_tokens"`
	ProviderAttempts   int     `json:"provider_attempts,omitempty"`
}

type Provider interface {
	Generate(ctx context.Context, req GenerateRequest) (GenerateResponse, error)
}

// ProviderDispatchGuard durably authorizes every provider request and records
// whether the response may be safely retried or must stay unresolved until a
// replay-safe checkpoint is persisted.
type ProviderDispatchGuard interface {
	BeginProviderDispatch(context.Context) error
	ProviderDispatchSucceeded(context.Context) error
	ProviderDispatchFailedDefinitely(context.Context) (bool, error)
}

type providerDispatchGuardContextKey struct{}

func WithProviderDispatchGuard(ctx context.Context, guard ProviderDispatchGuard) context.Context {
	return context.WithValue(ctx, providerDispatchGuardContextKey{}, guard)
}

func ProviderDispatchGuardFromContext(ctx context.Context) ProviderDispatchGuard {
	guard, _ := ctx.Value(providerDispatchGuardContextKey{}).(ProviderDispatchGuard)
	return guard
}

// GuardProvider wraps every concrete provider invocation. It descends through
// RetryingProvider so each HTTP retry crosses the durable dispatch boundary.
func GuardProvider(provider Provider, guard ProviderDispatchGuard) Provider {
	if provider == nil || guard == nil {
		return provider
	}
	if retrying, ok := provider.(*RetryingProvider); ok {
		return &RetryingProvider{delegate: GuardProvider(retrying.delegate, guard), maxRetry: retrying.maxRetry}
	}
	return &guardedProvider{delegate: provider, guard: guard}
}

type guardedProvider struct {
	delegate Provider
	guard    ProviderDispatchGuard
}

func (p *guardedProvider) Generate(ctx context.Context, req GenerateRequest) (GenerateResponse, error) {
	if err := p.guard.BeginProviderDispatch(ctx); err != nil {
		return GenerateResponse{}, err
	}
	response, callErr := p.delegate.Generate(ctx, req)
	if callErr == nil {
		if err := p.guard.ProviderDispatchSucceeded(ctx); err != nil {
			return GenerateResponse{}, &OutcomeUnknownError{Cause: err}
		}
		return response, nil
	}
	if IsDefinitelyPreDispatch(callErr) || isDefiniteHTTPResponse(callErr) {
		resolved, err := p.guard.ProviderDispatchFailedDefinitely(ctx)
		if err != nil {
			return GenerateResponse{}, &OutcomeUnknownError{Cause: errors.Join(callErr, err)}
		}
		if resolved {
			return GenerateResponse{}, callErr
		}
		// An earlier successful provider response in this attempt is still only
		// in memory. A later definite error cannot make that earlier work safe to
		// replay, so retain fail-closed semantics.
		return GenerateResponse{}, &OutcomeUnknownError{Cause: callErr}
	}
	return GenerateResponse{}, &OutcomeUnknownError{Cause: callErr}
}

func isDefiniteHTTPResponse(err error) bool {
	var httpErr *HTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode >= 100 && httpErr.StatusCode <= 599
}
