package app

import (
	"context"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
	"os"
	"testing"
	"time"
)

func TestRealDataSmoke(t *testing.T) {
	if os.Getenv("MYTOKEN_SMOKE") != "1" {
		t.Skip("opt-in: MYTOKEN_SMOKE=1 MYTOKEN_HOME=$(mktemp -d) go test -run TestRealDataSmoke -v ./internal/app")
	}
	a, e := Open()
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	ctx := context.Background()
	first := time.Now()
	if e = a.Scanner.Scan(ctx); e != nil {
		t.Fatal(e)
	}
	firstDuration := time.Since(first)
	for _, h := range []model.Harness{model.ClaudeCode, model.Codex, model.DSH, model.Pi} {
		v, e := a.Query.Totals(ctx, query.Filter{Harnesses: []model.Harness{h}})
		if e != nil {
			t.Fatal(e)
		}
		t.Logf("harness=%s events=%d sessions=%d", h, v.Requests, v.Sessions)
	}
	second := time.Now()
	if e = a.Scanner.Scan(ctx); e != nil {
		t.Fatal(e)
	}
	t.Logf("first_scan=%s incremental_scan=%s", firstDuration, time.Since(second))
}
