// Package model holds the shared, frozen types of MyToken!!!!!.
// Changing any exported signature here requires lead approval (docs/SPEC.md §3).
package model

import "time"

// Harness identifies the AI coding tool that produced a log.
type Harness string

const (
	ClaudeCode    Harness = "claude-code"
	Codex         Harness = "codex"
	Gemini        Harness = "gemini"
	DSH           Harness = "dsh"
	OpenCode      Harness = "opencode"
	Crush         Harness = "crush"
	Cline         Harness = "cline"
	Roo           Harness = "roo"
	Kilo          Harness = "kilo"
	Pi            Harness = "pi"
	Grok          Harness = "grok"
	OpenClaw      Harness = "openclaw"
	Droid         Harness = "droid"
	Goose         Harness = "goose"
	Forge         Harness = "forge"
	ClaudeDesktop Harness = "claude-desktop"
	Qwen          Harness = "qwen"
	Kimi          Harness = "kimi"
	OpenClaude    Harness = "openclaude"
	WorkBuddy     Harness = "workbuddy"
	CodeBuddy     Harness = "codebuddy"
	Hermes        Harness = "hermes"
)

// DisplayName is the human label of a harness.
func (h Harness) DisplayName() string {
	switch h {
	case ClaudeCode:
		return "Claude Code"
	case Codex:
		return "Codex"
	case Gemini:
		return "Gemini CLI"
	case DSH:
		return "DeepSeek Harness"
	case OpenCode:
		return "OpenCode"
	case Crush:
		return "Crush"
	case Cline:
		return "Cline"
	case Roo:
		return "Roo Code"
	case Kilo:
		return "Kilo Code"
	case Pi:
		return "Pi"
	case Grok:
		return "Grok CLI"
	case OpenClaw:
		return "OpenClaw"
	case Droid:
		return "Droid"
	case Goose:
		return "Goose"
	case Forge:
		return "Forge"
	case WorkBuddy:
		return "WorkBuddy"
	case CodeBuddy:
		return "CodeBuddy"
	case Hermes:
		return "Hermes"
	case ClaudeDesktop:
		return "Claude Desktop"
	case Qwen:
		return "Qwen Code"
	case Kimi:
		return "Kimi CLI"
	case OpenClaude:
		return "OpenClaude"
	}
	return string(h)
}

// Known reports whether h is one of the harnesses MyToken reads.
func (h Harness) Known() bool { return h != "" && h.DisplayName() != string(h) }

// Tokens are the five token classes. Parsers must not double count:
// if a log's output already includes reasoning, Output must exclude it.
type Tokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
	Reasoning  int64 `json:"reasoning"`
}

// Total sums all five classes.
func (t Tokens) Total() int64 {
	return t.Input + t.Output + t.CacheRead + t.CacheWrite + t.Reasoning
}

// Add returns t+o.
func (t Tokens) Add(o Tokens) Tokens {
	return Tokens{
		Input:      t.Input + o.Input,
		Output:     t.Output + o.Output,
		CacheRead:  t.CacheRead + o.CacheRead,
		CacheWrite: t.CacheWrite + o.CacheWrite,
		Reasoning:  t.Reasoning + o.Reasoning,
	}
}

// IsZero reports whether all classes are zero.
func (t Tokens) IsZero() bool { return t == Tokens{} }

// AttribSource records how a provider was determined.
type AttribSource string

const (
	AttribLog      AttribSource = "log"
	AttribCCSwitch AttribSource = "cc-switch"
	AttribConfig   AttribSource = "config-timeline"
	AttribInferred AttribSource = "inferred"
	AttribUserRule AttribSource = "user-rule"
)

// Boundary is an explicitly observed context lifecycle change before a request.
// Empty means unknown; missing records and model changes are not boundaries.
type Boundary string

const (
	BoundaryCompact Boundary = "compact"
	BoundaryClear   Boundary = "clear"
	BoundaryResume  Boundary = "resume"
)

// Bill preserves a tool's explicitly reported charge in its native unit. It is
// independent of CostUSD: credits are never converted to or added to USD.
type Bill struct {
	Amount float64 `json:"amount"`
	Unit   string  `json:"unit"`
}

// UsageEvent is one model request's usage, the single source of truth.
type UsageEvent struct {
	Harness     Harness   `json:"harness"`
	DedupKey    string    `json:"dedupKey"`  // stable; harness request/message id, else sha1(file+offset)
	SessionID   string    `json:"sessionId"` // subagents use their own id
	ParentID    string    `json:"parentId,omitempty"`
	ProjectPath string    `json:"projectPath,omitempty"`
	Timestamp   time.Time `json:"timestamp"` // UTC
	Model       string    `json:"model"`
	Provider    string    `json:"provider,omitempty"` // set only when the log itself says so
	BaseURL     string    `json:"baseUrl,omitempty"`
	Tokens      Tokens    `json:"tokens"`
	CostUSD     *float64  `json:"costUsd,omitempty"`   // cost reported by the log, if any
	RequestID   string    `json:"requestId,omitempty"` // upstream request/response ID, never a local fallback
	Boundary    Boundary  `json:"boundary,omitempty"`
	Bill        *Bill     `json:"bill,omitempty"`
}

// SessionMeta describes a session. Title is the only conversation text stored.
type SessionMeta struct {
	Harness   Harness   `json:"harness"`
	SessionID string    `json:"sessionId"`
	ParentID  string    `json:"parentId,omitempty"`
	Title     string    `json:"title,omitempty"` // first user message, ≤60 runes, no newlines
	Project   string    `json:"project,omitempty"`
	StartedAt time.Time `json:"startedAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}
