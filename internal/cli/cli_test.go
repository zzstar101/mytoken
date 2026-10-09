package cli

import (
	"bytes"
	"encoding/json"
	"github.com/zzstar/mytoken/internal/app"
	"github.com/zzstar/mytoken/internal/harness"
	"github.com/zzstar/mytoken/internal/model"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPricesCLI(t *testing.T) {
	t.Setenv("MYTOKEN_HOME", t.TempDir())
	var out, errout bytes.Buffer
	args := []string{"prices", "set", "--provider", "Relay", "--model", "custom", "--input", "0", "--multiplier", "0.5"}
	if code := run(args, &out, &errout); code != 0 {
		t.Fatalf("code=%d err=%s", code, errout.String())
	}
	out.Reset()
	if code := run([]string{"prices", "list"}, &out, &errout); code != 0 {
		t.Fatal(code, errout.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("Relay")) || !bytes.Contains(out.Bytes(), []byte("custom")) {
		t.Fatal(out.String())
	}
	for _, args := range [][]string{{"prices", "set", "--input", "2"}, {"prices", "set", "--provider", "Relay", "--input", "-1"}, {"prices", "nonsense"}} {
		if code := run(args, &out, &errout); code == 0 {
			t.Fatal(args)
		}
	}
}

func TestStatsJSON(t *testing.T) {
	t.Setenv("MYTOKEN_HOME", t.TempDir())
	a, e := app.Open()
	if e != nil {
		t.Fatal(e)
	}
	e = a.Store.Commit(t.Context(), model.Codex, "test", harness.Batch{Events: []model.UsageEvent{{Harness: model.Codex, DedupKey: "e", SessionID: "s", Model: "gpt-5", Timestamp: time.Now(), Tokens: model.Tokens{Input: 20}}}}, nil)
	a.Close()
	if e != nil {
		t.Fatal(e)
	}
	var out, errout bytes.Buffer
	if code := run([]string{"stats", "--json", "--by", "model", "--since", "7d"}, &out, &errout); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errout.String())
	}
	var data struct {
		Totals struct {
			Requests int64 `json:"requests"`
		} `json:"totals"`
		Rows []struct {
			Key string `json:"key"`
		} `json:"rows"`
	}
	if e = json.Unmarshal(out.Bytes(), &data); e != nil || data.Totals.Requests != 1 || len(data.Rows) != 1 || data.Rows[0].Key != "gpt-5" {
		t.Fatalf("json %s error %v", out.String(), e)
	}
	_ = os.Remove(filepath.Join(os.Getenv("MYTOKEN_HOME"), "nothing"))
}
