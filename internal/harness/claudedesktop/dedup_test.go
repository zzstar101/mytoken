package claudedesktop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
)

// The Cowork usage ledger and the Claude Code transcript of the same session
// both contain the same request: the ledger bills it, the transcript repeats it.
// Only the linked pair is deduplicated, and only when the token usage matches.

const dedupSession = "cd-22222222-2222-2222-2222-222222222222"

var dedupBase = time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)

func TestLedgerDedupDropsCoveredTranscriptRequest(t *testing.T) {
	root, transcript := dedupWorkspace(t, dedupSession, 30)
	want := []string{
		transcriptKey("w"),
		transcriptKey("y"),
		transcriptKey("z"),
	}
	if got := parseTranscriptKeys(t, root, transcript); !slices.Equal(got, want) {
		t.Fatalf("linked transcript events = %v, want %v (the ledger already bills req-x)", got, want)
	}
}

// Without the workspace metadata link the two files cannot be related, so
// nothing may be dropped.
func TestLedgerDedupNeedsSessionLink(t *testing.T) {
	root, transcript := dedupWorkspace(t, "cd-33333333-3333-3333-3333-333333333333", 30)
	want := []string{
		transcriptKey("w"),
		transcriptKey("x"),
		transcriptKey("y"),
		transcriptKey("z"),
	}
	if got := parseTranscriptKeys(t, root, transcript); !slices.Equal(got, want) {
		t.Fatalf("unlinked transcript events = %v, want %v", got, want)
	}
}

// req-x is only covered because the ledger records the billed output (30) while
// the transcript records 50 including 20 thinking tokens, so the emitted event
// matches. A ledger record that matches neither the raw nor the emitted usage
// must not drop anything.
func TestLedgerDedupNeedsMatchingUsage(t *testing.T) {
	root, transcript := dedupWorkspace(t, dedupSession, 999)
	want := []string{
		transcriptKey("w"),
		transcriptKey("x"),
		transcriptKey("y"),
		transcriptKey("z"),
	}
	if got := parseTranscriptKeys(t, root, transcript); !slices.Equal(got, want) {
		t.Fatalf("transcript events = %v, want %v (the ledger usage does not match)", got, want)
	}
}

func dedupWorkspace(t *testing.T, cliSessionID string, billedOutput int64) (root, transcript string) {
	t.Helper()
	root = t.TempDir()
	workspace := filepath.Join(root, "3f9c1d2e", "00000000")
	if err := os.MkdirAll(filepath.Join(workspace, "usage-ledger"), 0o755); err != nil {
		t.Fatalf("mkdir usage-ledger: %v", err)
	}
	writeFixtureFile(t, filepath.Join(workspace, "local_a.json"),
		fmt.Sprintf(`{"sessionId":"local_a","cliSessionId":%q}`, cliSessionID))

	transcript = filepath.Join(workspace, "short", ".claude", "projects", "proj", dedupSession+".jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
		t.Fatalf("mkdir transcript: %v", err)
	}
	lines := []string{
		fmt.Sprintf(`{"type":"user","timestamp":%q,"sessionId":%q,"cwd":"/Users/demo/proj","uuid":"u-0","message":{"role":"user","content":"dedup probe"}}`,
			dedupBase.Format(time.RFC3339), dedupSession),
		dedupAssistant(t, time.Second, "x", 100, 50, 10, 5, 20, 30),
		dedupAssistant(t, 2*time.Second, "y", 200, 20, 0, 0, 0, 20),
		dedupAssistant(t, 3*time.Second, "z", 300, 30, 0, 0, 0, 30),
		dedupAssistant(t, time.Minute, "w", 400, 40, 0, 0, 0, 40),
	}
	writeFixtureFile(t, transcript, strings.Join(lines, "\n")+"\n")

	ledger := []string{
		// Bills req-x: input and caches as the transcript reports them, output
		// as the emitted event (the transcript's raw output includes thinking).
		dedupLedger(dedupBase.Add(1400*time.Millisecond), 100, billedOutput, 10, 5),
		// Same second as req-z but different usage: not a duplicate.
		dedupLedger(dedupBase.Add(3*time.Second), 300, 31, 0, 0),
		// Same usage as req-w but far outside the match window.
		dedupLedger(dedupBase.Add(30*time.Second), 400, 40, 0, 0),
	}
	writeFixtureFile(t, filepath.Join(workspace, "usage-ledger", "2026-10-03.ndjson"), strings.Join(ledger, "\n")+"\n")
	return root, transcript
}

// dedupAssistant renders one transcript line; emitted is the output the parser
// is expected to report after removing thinking tokens.
func dedupAssistant(t *testing.T, at time.Duration, id string, input, output, cacheRead, cacheWrite, thinking, emitted int64) string {
	t.Helper()
	if output-thinking != emitted {
		t.Fatalf("test premise: output %d - thinking %d != emitted %d", output, thinking, emitted)
	}
	line := map[string]any{
		"type":      "assistant",
		"timestamp": dedupBase.Add(at).Format(time.RFC3339Nano),
		"sessionId": dedupSession,
		"cwd":       "/Users/demo/proj",
		"uuid":      "u-" + id,
		"requestId": "req-" + id,
		"message": map[string]any{
			"id":    "msg_" + id,
			"model": "claude-sonnet-4-5",
			"role":  "assistant",
			"usage": map[string]any{
				"input_tokens":                input,
				"output_tokens":               output,
				"cache_read_input_tokens":     cacheRead,
				"cache_creation_input_tokens": cacheWrite,
				"output_tokens_details":       map[string]any{"thinking_tokens": thinking},
			},
			"content": []map[string]any{{"type": "text", "text": "reply"}},
		},
	}
	raw, err := json.Marshal(line)
	if err != nil {
		t.Fatalf("marshal transcript line: %v", err)
	}
	return string(raw)
}

func dedupLedger(at time.Time, input, output, cacheRead, cacheWrite int64) string {
	return fmt.Sprintf(`{"surface":"cowork","sessionId":"local_a","ts":%d,"models":{"claude-sonnet-4-5":{"inputTokens":%d,"outputTokens":%d,"cacheReadTokens":%d,"cacheWriteTokens":%d,"cost":{"usd":0.01}}}}`,
		at.UnixMilli(), input, output, cacheRead, cacheWrite)
}

func transcriptKey(id string) string {
	return transcriptPrefix + dedupSession + ":msg_" + id + ":req-" + id
}

func parseTranscriptKeys(t *testing.T, root, path string) []string {
	t.Helper()
	p := NewWithRoots(root)
	b, err := p.Parse(context.Background(), harness.Source{Path: path, Kind: kindJSONL}, harness.Cursor{})
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	keys := make([]string, 0, len(b.Events))
	for _, e := range b.Events {
		keys = append(keys, e.DedupKey)
	}
	slices.Sort(keys)
	return keys
}
