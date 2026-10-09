package scan

import (
	"context"
	"errors"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type cancelParser struct{ entered chan struct{} }

func (p cancelParser) Harness() model.Harness { return model.Codex }
func (p cancelParser) Roots() []string        { return nil }
func (p cancelParser) Discover(context.Context) ([]harness.Source, error) {
	return make([]harness.Source, 10000), nil
}
func (p cancelParser) Parse(ctx context.Context, _ harness.Source, _ harness.Cursor) (harness.Batch, error) {
	select {
	case p.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return harness.Batch{}, ctx.Err()
}
func TestRunCancellationStopsDispatch(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := cancelParser{entered: make(chan struct{}, 1)}
	sc := New(st, nil, nil, p)
	sc.workers = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- sc.Run(ctx) }()
	select {
	case <-p.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("parse did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return promptly")
	}
	done, _ := sc.Progress()
	if done > 2 {
		t.Fatalf("dispatched %d sources after cancellation", done)
	}
}

type parallelParser struct {
	entered chan struct{}
	release chan struct{}
}

func (p parallelParser) Harness() model.Harness { return model.Codex }
func (p parallelParser) Roots() []string        { return nil }
func (p parallelParser) Discover(context.Context) ([]harness.Source, error) {
	return []harness.Source{{Path: "a"}, {Path: "b"}, {Path: "c"}, {Path: "d"}}, nil
}
func (p parallelParser) Parse(ctx context.Context, src harness.Source, _ harness.Cursor) (harness.Batch, error) {
	p.entered <- struct{}{}
	select {
	case <-ctx.Done():
		return harness.Batch{}, ctx.Err()
	case <-p.release:
	}
	return harness.Batch{Events: []model.UsageEvent{{DedupKey: src.Path, SessionID: src.Path}}, Next: harness.Cursor{Offset: 1}}, nil
}
func TestParallelParsing(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := parallelParser{make(chan struct{}, 4), make(chan struct{})}
	sc := New(st, nil, nil, p)
	sc.workers = 4
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- sc.Scan(ctx) }()
	for i := 0; i < 4; i++ {
		select {
		case <-p.entered:
		case <-ctx.Done():
			t.Fatal("workers did not parse concurrently")
		}
	}
	close(p.release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.DB().QueryRow("SELECT count(*) FROM events").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("events=%d", n)
	}
}

type testParser struct{ root string }

func (p testParser) Harness() model.Harness { return model.Harness("fixture") }
func (p testParser) Roots() []string        { return []string{p.root} }
func (p testParser) Discover(ctx context.Context) ([]harness.Source, error) {
	return []harness.Source{{Path: filepath.Join(p.root, "log"), Kind: "jsonl"}}, nil
}
func (p testParser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	raw, e := os.ReadFile(src.Path)
	if e != nil {
		return harness.Batch{}, e
	}
	b := harness.Batch{Next: harness.Cursor{Offset: int64(len(raw)), Size: int64(len(raw))}}
	for i := cur.Offset; i < int64(len(raw)); i++ {
		if raw[i] == '\n' {
			b.Events = append(b.Events, model.UsageEvent{DedupKey: string(raw[i-1]), SessionID: "s", Model: "gpt-5", Timestamp: time.Now(), Tokens: model.Tokens{Input: 1}})
		}
	}
	return b, nil
}
func TestIncrementalAppendNoDuplicates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	os.WriteFile(path, []byte("a\n"), 0600)
	st, e := store.Open(filepath.Join(dir, "db"))
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	sc := New(st, nil, nil, testParser{dir})
	ctx := context.Background()
	if e = sc.Scan(ctx); e != nil {
		t.Fatal(e)
	}
	f, e := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	f.WriteString("b\n")
	f.Close()
	if e = sc.Scan(ctx); e != nil {
		t.Fatal(e)
	}
	if e = sc.Scan(ctx); e != nil {
		t.Fatal(e)
	}
	var n int
	st.DB().QueryRow("SELECT count(*) FROM events").Scan(&n)
	if n != 2 {
		t.Fatalf("events=%d", n)
	}
	done, total := sc.Progress()
	if done != 1 || total != 1 {
		t.Fatalf("progress %d %d", done, total)
	}
	if e = sc.Rebuild(ctx); e != nil {
		t.Fatal(e)
	}
	st.DB().QueryRow("SELECT count(*) FROM events").Scan(&n)
	if n != 2 {
		t.Fatalf("rebuild %d", n)
	}
}
