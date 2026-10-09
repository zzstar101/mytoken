package codex

import (
	"context"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBoundaryPersistsAcrossIncrementalBatch(t *testing.T) {
	raw, err := os.ReadFile("testdata/signals.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(raw), "\n")
	path := filepath.Join(t.TempDir(), "signals.jsonl")
	prefix := `{"type":"padding","ignored":"` + strings.Repeat("x", 4200) + "\"}\n" + strings.Join(lines[:4], "")
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
	if len(first.Events) != 1 || first.Events[0].RequestID != "" || first.Events[0].Boundary != "" {
		t.Fatalf("initial %+v", first.Events)
	}
	idle, err := p.Parse(ctx, src, first.Next)
	if err != nil {
		t.Fatal(err)
	}
	if idle.Next != first.Next {
		t.Fatalf("idle changed %s => %s", first.Next.Extra, idle.Next.Extra)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(strings.Join(lines[4:], ""))
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.Parse(ctx, src, first.Next)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Events) != 2 || second.Events[0].Boundary != model.BoundaryCompact || second.Events[1].Boundary != "" {
		t.Fatalf("boundaries %+v", second.Events)
	}
	if second.Events[0].RequestID != "" || second.Events[1].RequestID != "req-explicit" {
		t.Fatalf("IDs %+v", second.Events)
	}
	for _, e := range second.Events {
		if e.Bill != nil || e.CostUSD != nil {
			t.Fatal("credit balance misreported as bill")
		}
	}
	full, err := p.Parse(ctx, src, harness.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Events) != 3 || full.Events[1].Boundary != model.BoundaryCompact {
		t.Fatalf("full replay %+v", full.Events)
	}
}
