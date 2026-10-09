package pi

import (
	"context"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRequestBoundarySignals(t *testing.T) {
	raw, err := os.ReadFile("testdata/signals.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(raw), "\n")
	path := filepath.Join(t.TempDir(), "signals.jsonl")
	prefix := lines[0] + `{"type":"padding","ignored":"` + strings.Repeat("x", 4200) + "\"}\n" + strings.Join(lines[1:3], "")
	if err = os.WriteFile(path, []byte(prefix), 0600); err != nil {
		t.Fatal(err)
	}
	p := NewWithRoots(filepath.Dir(path))
	src := harness.Source{Path: path, Kind: "jsonl"}
	ctx := context.Background()
	first, err := p.Parse(ctx, src, harness.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Events) != 1 || first.Events[0].RequestID != "resp-before" || first.Events[0].CostUSD == nil || *first.Events[0].CostUSD != 0.25 || first.Events[0].Bill != nil {
		t.Fatalf("initial %+v", first.Events)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(strings.Join(lines[3:], ""))
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.Parse(ctx, src, first.Next)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Events) != 2 || second.Events[0].Boundary != model.BoundaryCompact || second.Events[0].RequestID != "resp-after" || second.Events[1].Boundary != "" || second.Events[1].RequestID != "" {
		t.Fatalf("signals %+v", second.Events)
	}
	idle, err := p.Parse(ctx, src, second.Next)
	if err != nil || idle.Next != second.Next {
		t.Fatalf("idle: %+v %v", idle.Next, err)
	}
}
