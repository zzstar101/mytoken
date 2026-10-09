package codex

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zzstar/mytoken/internal/harness"
)

func TestUnchangedReturnsOnlyCursor(t *testing.T) {
	p := NewWithRoots(fixtureRoot)
	first := parseFile(t, p, mainRollout, harness.Cursor{})
	again := parseFile(t, p, mainRollout, first.Next)
	if len(again.Events) != 0 || len(again.Sessions) != 0 || again.Next != first.Next {
		t.Fatalf("unchanged source returned a write: %+v", again)
	}
}

func TestLongSkippedLineCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-test.jsonl")
	line := `{"timestamp":"2026-01-02T05:00:00Z","ordinal":12,"type":"response_item","payload":{"type":"function_call_output","output":"` + strings.Repeat("x", 3<<20) + `"}}`
	if err := os.WriteFile(path, []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
	p := NewWithRoots(filepath.Dir(path))
	partial := parseFile(t, p, path, harness.Cursor{})
	if partial.Next.Offset != 0 {
		t.Fatal("consumed incomplete line")
	}
	if err := os.WriteFile(path, []byte(line+"\n"+fillerLine), 0600); err != nil {
		t.Fatal(err)
	}
	complete := parseFile(t, p, path, partial.Next)
	if complete.Next.Offset != int64(len(line)+1+len(fillerLine)) {
		t.Fatalf("offset %d", complete.Next.Offset)
	}
	if len(complete.Events) != 0 || len(complete.Sessions) != 1 || complete.Sessions[0].UpdatedAt.IsZero() {
		t.Fatalf("batch %+v", complete)
	}
}

func TestFastAndDecodedRolloutMatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), filepath.Base(mainRollout))
	raw, err := os.ReadFile(mainRollout)
	if err != nil {
		t.Fatal(err)
	}
	// Exercise an ignored record spanning several reader buffers, followed by
	// the real fixture's metadata, titles and token-count records.
	prefix := `{"timestamp":"2026-01-02T05:00:00Z","ordinal":12,"type":"response_item","payload":{"type":"function_call_output","output":"` + strings.Repeat("x", 3<<20) + `"}}` + "\n"
	raw = append([]byte(prefix), raw...)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	p := NewWithRoots(filepath.Dir(path))
	fast := parseFile(t, p, path, harness.Cursor{})
	// Leading whitespace is legal JSON but disables envelope recognition.
	slowRaw := " " + strings.ReplaceAll(string(raw), "\n", "\n ")
	if err := os.WriteFile(path, []byte(slowRaw), 0600); err != nil {
		t.Fatal(err)
	}
	slow := parseFile(t, p, path, harness.Cursor{})
	if !reflect.DeepEqual(fast.Events, slow.Events) || !reflect.DeepEqual(fast.Sessions, slow.Sessions) || fast.Next.Extra != slow.Next.Extra {
		t.Fatalf("fast and decoded rollout differ: fast=%+v slow=%+v", fast, slow)
	}
}

func TestSkippable(t *testing.T) {
	const stamp = `2026-01-02T05:00:00.000Z`
	for _, tc := range []struct {
		name, rest string
		skip       bool
	}{
		{"tool", `,"type":"response_item","payload":{"type":"function_call","arguments":"large"}}`, true},
		{"ordinal", `,"ordinal":123,"type":"event_msg","payload":{"type":"agent_message"}}`, true},
		{"usage", `,"ordinal":123,"type":"event_msg","payload":{"type":"token_count"}}`, false},
		{"user", `,"type":"event_msg","payload":{"type":"user_message"}}`, false},
		{"message", `,"type":"response_item","payload":{"type":"message"}}`, false},
		{"escaped_usage", `,"type":"event_msg","payload":{"type":"\u0074oken_count"}}`, false},
		{"usage_record", `,"type":"token_usage_record","payload":{"type":"other"}}`, false},
		{"nested_type", `,"extra":{"type":"event_msg","payload":{"type":"agent_message"}},"type":"event_msg","payload":{"type":"token_count"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, ok := skippable([]byte(`{"timestamp":"` + stamp + `"` + tc.rest))
			if ok != tc.skip {
				t.Fatalf("skip = %v, want %v", ok, tc.skip)
			}
			want, _ := time.Parse(time.RFC3339Nano, stamp)
			if ok && !ts.Equal(want) {
				t.Fatalf("timestamp = %v", ts)
			}
		})
	}
}
