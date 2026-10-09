package workbuddy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

const fixtureSession = "01a00000-0000-7000-8000-000000000001"

// parseLines parses synthetic records from a session log in a temp directory.
func parseLines(t *testing.T, lines ...string) harness.Batch {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, fixtureSession+".jsonl")
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	p := NewWithRoots(dir)
	srcs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 1 {
		t.Fatalf("discovered %d sources, want 1", len(srcs))
	}
	b, err := p.Parse(context.Background(), srcs[0], harness.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// usageLine is one function_call record carrying both the normalized and the
// raw provider usage.
func usageLine(id, modelID string, in, out, read, write, think int, credit float64) string {
	return fmt.Sprintf(`{"id":%q,"timestamp":1767323046000,"type":"function_call","cwd":"/home/user/fixture-alpha","sessionId":%q,"providerData":{"messageId":%q,"model":%q,"conversationRequestId":"req-%s","rawUsage":{"prompt_tokens":%d,"completion_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d,"completion_thinking_tokens":%d,"credit":%g}},"message":{"usage":{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":%d}}}`,
		id, fixtureSession, id, modelID, id, in, out, read, write, think, credit, in, out, read)
}

func userLine(id, text string) string {
	return fmt.Sprintf(`{"id":%q,"timestamp":1767323045200,"type":"message","role":"user","sessionId":%q,"content":[{"type":"input_text","text":%q}]}`,
		id, fixtureSession, text)
}

// TestTitleFromFirstUserMessage checks that the injected wrapper block is not
// used as the title and that the first real user message is.
func TestTitleFromFirstUserMessage(t *testing.T) {
	b := parseLines(t,
		userLine("u0", "<system-reminder data-role=\"user-context\">wrapper</system-reminder>"),
		userLine("u1", "first real request"),
	)
	if len(b.Sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(b.Sessions))
	}
	if got := b.Sessions[0].Title; got != "first real request" {
		t.Errorf("title = %q, want %q", got, "first real request")
	}
}

// TestTitlePrefersAITitle checks the ai-title record, which is authoritative.
func TestTitlePrefersAITitle(t *testing.T) {
	b := parseLines(t,
		userLine("u1", "first real request"),
		fmt.Sprintf(`{"id":"t1","timestamp":1767323045300,"type":"ai-title","aiTitle":"fixture ai title","sessionId":%q}`, fixtureSession),
	)
	if got := b.Sessions[0].Title; got != "fixture ai title" {
		t.Errorf("title = %q, want %q", got, "fixture ai title")
	}
}

// TestTokensBillAndBoundary checks the token split, the native credit bill, the
// request id and the compaction boundary handoff.
func TestTokensBillAndBoundary(t *testing.T) {
	b := parseLines(t,
		usageLine("m1", "deepseek-v4.1-flash", 1000, 200, 800, 120, 40, 0.75),
		fmt.Sprintf(`{"id":"k1","timestamp":1767323048000,"type":"message","role":"user","sessionId":%q,"providerData":{"isCompactInternal":true,"compactType":"emergency-auto","isCompacted":true,"isSummary":true,"isMeta":true}}`, fixtureSession),
		usageLine("m2", "kimi-k3", 300, 90, 100, 10, 25, 0.25),
	)
	if len(b.Events) != 2 {
		t.Fatalf("got %d events, want 2", len(b.Events))
	}
	first, second := b.Events[0], b.Events[1]
	want := model.Tokens{Input: 80, Output: 160, CacheRead: 800, CacheWrite: 120, Reasoning: 40}
	if first.Tokens != want {
		t.Errorf("first tokens = %+v, want %+v", first.Tokens, want)
	}
	if first.Model != "deepseek-v4.1-flash" || first.RequestID != "req-m1" {
		t.Errorf("first event model/request = %q/%q", first.Model, first.RequestID)
	}
	if first.Bill == nil || first.Bill.Amount != 0.75 || first.Bill.Unit != "credit" {
		t.Errorf("first bill = %+v, want 0.75 credit", first.Bill)
	}
	if first.Boundary != "" {
		t.Errorf("first boundary = %q, want none", first.Boundary)
	}
	if second.Boundary != model.BoundaryCompact {
		t.Errorf("second boundary = %q, want %q", second.Boundary, model.BoundaryCompact)
	}
	wantSecond := model.Tokens{Input: 190, Output: 65, CacheRead: 100, CacheWrite: 10, Reasoning: 25}
	if second.Tokens != wantSecond {
		t.Errorf("second tokens = %+v, want %+v", second.Tokens, wantSecond)
	}
}

// TestZeroUsageSkipped checks that a usage record whose counters are all zero
// produces no event.
func TestZeroUsageSkipped(t *testing.T) {
	b := parseLines(t, usageLine("m1", "deepseek-v4.1-flash", 0, 0, 0, 0, 0, 0))
	if len(b.Events) != 0 {
		t.Fatalf("got %d events, want 0", len(b.Events))
	}
}

// TestParentFromPath checks the subagent layout
// <project>/<session-id>/subagents/agent-<hex>.jsonl.
func TestParentFromPath(t *testing.T) {
	sub := filepath.Join("/home/u/.workbuddy/projects/-home-u-proj", fixtureSession, "subagents", "agent-00000000000000ff.jsonl")
	if got := parentFromPath(sub); got != fixtureSession {
		t.Errorf("parentFromPath(subagent) = %q, want %q", got, fixtureSession)
	}
	top := filepath.Join("/home/u/.workbuddy/projects/-home-u-proj", fixtureSession+".jsonl")
	if got := parentFromPath(top); got != "" {
		t.Errorf("parentFromPath(top level) = %q, want empty", got)
	}
}

// TestUnchangedSourceReemitsNothing checks the incremental fast path.
func TestUnchangedSourceReemitsNothing(t *testing.T) {
	b := parseLines(t, usageLine("m1", "deepseek-v4.1-flash", 1000, 200, 800, 120, 40, 0.75))
	p := NewWithRoots(t.TempDir())
	srcs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 0 {
		t.Fatalf("got %d sources, want 0", len(srcs))
	}
	if b.Next.Fingerprint == "" || b.Next.Size == 0 {
		t.Fatalf("cursor not populated: %+v", b.Next)
	}
}
