package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

type GuardConfig struct {
	MaxSteps       int
	MaxToolCalls   int
	MaxRepeatCalls int
}

func DefaultGuardConfig() GuardConfig {
	return GuardConfig{
		MaxSteps:       8,
		MaxToolCalls:   12,
		MaxRepeatCalls: 2,
	}
}

type AgentGuard struct {
	cfg            GuardConfig
	stepCount      int
	toolCallCount  int
	callHistory    map[string]int
	lastCallHash   string
	consecutiveSame int
}

func NewAgentGuard(cfg GuardConfig) *AgentGuard {
	return &AgentGuard{
		cfg:         cfg,
		callHistory: make(map[string]int),
	}
}

func (g *AgentGuard) RecordStep() error {
	g.stepCount++
	if g.stepCount > g.cfg.MaxSteps {
		return fmt.Errorf("agent step limit exceeded (max: %d)", g.cfg.MaxSteps)
	}
	return nil
}

func (g *AgentGuard) RecordToolCall(name, argsJSON string) error {
	g.toolCallCount++
	if g.toolCallCount > g.cfg.MaxToolCalls {
		return fmt.Errorf("agent tool call limit exceeded (max: %d)", g.cfg.MaxToolCalls)
	}

	h := sha256.Sum256([]byte(name + ":" + argsJSON))
	callHash := hex.EncodeToString(h[:])

	if callHash == g.lastCallHash {
		g.consecutiveSame++
		if g.consecutiveSame >= g.cfg.MaxRepeatCalls {
			return fmt.Errorf("repeated identical tool call detected: %s", name)
		}
	} else {
		g.consecutiveSame = 0
		g.lastCallHash = callHash
	}

	g.callHistory[callHash]++
	if g.callHistory[callHash] > 3 {
		return fmt.Errorf("duplicate tool call threshold exceeded for %s", name)
	}

	return nil
}
