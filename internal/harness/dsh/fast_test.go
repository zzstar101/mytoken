package dsh

import (
	"reflect"
	"strings"
	"testing"
)

func TestSkippedRecordMatchesDecoded(t *testing.T) {
	for _, kind := range []string{"tool/call", "tool/result", "turn/end", "unknown/future"} {
		t.Run(kind, func(t *testing.T) {
			line := `{"type":"` + kind + `","seq":12,"time":1767326400000,"data":{"text":"body"}}`
			if _, ok := skippable([]byte(line)); !ok {
				t.Fatal("expected fast path")
			}
			fast, slow := newSession("/test/session.jsonl"), newSession("/test/session.jsonl")
			if err := fast.consume([]byte(line)); err != nil {
				t.Fatal(err)
			}
			if err := slow.consume([]byte(strings.Replace(line, `{"type":`, `{ "type":`, 1))); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(fast, slow) {
				t.Fatalf("fast %+v != slow %+v", fast, slow)
			}
		})
	}
	for _, kind := range []string{"session", "session/title", "user/message", "assistant/message", "compaction/summary"} {
		line := `{"type":"` + kind + `","seq":12,"time":1767326400000,"data":{}}`
		if _, ok := skippable([]byte(line)); ok {
			t.Fatalf("skipped %s", kind)
		}
	}
	for _, line := range []string{`{"type":"tool/result","seq":null,"time":1,"data":{}}`, `{"type":"tool/result","seq":1,"time":"1","data":{}}`, `{"extra":{"type":"tool/result"},"seq":1,"time":1,"data":{}}`} {
		if _, ok := skippable([]byte(line)); ok {
			t.Fatalf("skipped foreign layout %s", line)
		}
	}
}
