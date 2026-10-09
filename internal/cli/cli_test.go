package cli

import (
	"bytes"
	"encoding/json"
	"github.com/zzstar101/mytoken/internal/app"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCommandSurface(t *testing.T) {
	t.Setenv("MYTOKEN_HOME", t.TempDir())
	for _, args := range [][]string{{"version"}, {"help"}, {"sessions", "--limit", "2", "--json"}, {"doctor", "--json"}, {"stats", "--by", "harness", "--json"}} {
		var out, errout bytes.Buffer
		if code := run(args, &out, &errout); code != 0 {
			t.Errorf("%v: code=%d stderr=%s", args, code, &errout)
		}
		if len(args) > 1 && args[len(args)-1] == "--json" && !json.Valid(out.Bytes()) {
			t.Errorf("%v: invalid JSON %s", args, &out)
		}
	}
	for _, args := range [][]string{{"version", "extra"}, {"help", "extra"}, {"sessions", "--limit", "-1"}, {"doctor", "extra"}} {
		var out, errout bytes.Buffer
		if code := run(args, &out, &errout); code != 2 {
			t.Errorf("%v: code=%d", args, code)
		}
	}
}

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
