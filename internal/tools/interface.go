package tools

import (
	"context"

	"repolens/internal/llm"
)

type Tool interface {
	Name() string
	Description() string
	Definition() llm.ToolDefinition
	Execute(ctx context.Context, argsJSON string) (string, error)
}
