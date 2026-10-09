package pricing

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestBuiltinLookupAgreesWithLookup is the contract that makes BuiltinLookup
// usable as the reconciliation baseline: for every name in the embedded catalog
// it must return exactly what a fresh, rule-free Pricer returns, including the
// normalized, path-prefixed and date-suffixed spellings a gateway logs.
func TestBuiltinLookupAgreesWithLookup(t *testing.T) {
	p := New("")
	var compact struct {
		Prices []Price        `json:"prices"`
		Models map[string]int `json:"models"`
	}
	if err := json.Unmarshal(snapshot, &compact); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(compact.Models))
	for k := range compact.Models {
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		t.Fatal("embedded catalog is empty")
	}
	for _, k := range keys {
		want, wantOK := p.Lookup(k)
		got, gotOK := BuiltinLookup(k)
		if gotOK != wantOK || !samePrice(got, want) {
			t.Fatalf("BuiltinLookup(%q) = %+v,%v; Lookup = %+v,%v", k, got, gotOK, want, wantOK)
		}
		// Spellings a relay is likely to log: a path prefix, a :latest tag and a
		// date suffix. The date suffix is the interesting one because it must
		// fall back to the undated entry.
		for _, variant := range []string{
			"anthropic/" + k,
			strings.TrimSuffix(k, "-latest") + "-latest",
			k + "-20260101",
			k + ":latest",
		} {
			want, wantOK := p.Lookup(variant)
			got, gotOK := BuiltinLookup(variant)
			if gotOK != wantOK || !samePrice(got, want) {
				t.Fatalf("BuiltinLookup(%q) = %+v,%v; Lookup = %+v,%v", variant, got, gotOK, want, wantOK)
			}
		}
	}
}

// samePrice compares prices by value; Reasoning is a pointer, so two entries
// that mean the same thing are not ==.
func samePrice(a, b Price) bool {
	if a.Input != b.Input || a.Output != b.Output || a.CacheRead != b.CacheRead || a.CacheWrite != b.CacheWrite {
		return false
	}
	if (a.Reasoning == nil) != (b.Reasoning == nil) {
		return false
	}
	return a.Reasoning == nil || *a.Reasoning == *b.Reasoning
}

// TestBuiltinLookupIgnoresRulesAndDisk is the other half of the contract: a
// rule, an override or a disk cache must never leak into the builtin answer.
func TestBuiltinLookupIgnoresRulesAndDisk(t *testing.T) {
	p := New("")
	price := 123.0
	p.SetRules([]Rule{{Model: "claude-sonnet-4-5", Input: &price, Output: &price, CacheRead: &price, CacheWrite: &price}})
	if v, ok := p.Lookup("claude-sonnet-4-5"); !ok || v.Input == 123 {
		t.Fatalf("Lookup = %+v,%v", v, ok)
	}
	builtin, ok := BuiltinLookup("claude-sonnet-4-5")
	if !ok {
		t.Fatal("BuiltinLookup lost a catalog model")
	}
	if builtin.Input == 123 {
		t.Fatalf("BuiltinLookup = %+v, a rule leaked into it", builtin)
	}
	if _, ok := BuiltinLookup("no-such-model-anywhere"); ok {
		t.Fatal("BuiltinLookup invented a price")
	}
}

// BenchmarkBuiltinIndex reports the one-time cost BuiltinLookup pays on its
// first call. It lives here rather than as a test assertion because wall-clock
// bounds in a test fail on a loaded machine for reasons that have nothing to do
// with the code; run it deliberately when the catalog or the index changes.
func BenchmarkBuiltinIndex(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if aliases := indexCatalog(snapshotPrices()); len(aliases) == 0 {
			b.Fatal("no aliases built")
		}
	}
}

func BenchmarkBuiltinLookup(b *testing.B) {
	// Warm the cache the way a long-lived process would.
	if _, ok := BuiltinLookup("claude-sonnet-4-5"); !ok {
		b.Fatal("model missing")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := BuiltinLookup("claude-sonnet-4-5-20260101"); !ok {
			b.Fatal("model missing")
		}
	}
}
