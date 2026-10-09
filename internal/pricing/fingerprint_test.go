package pricing

import (
	"testing"
	"time"
)

func TestFingerprintChangesOnlyWithEvaluationInputs(t *testing.T) {
	p := New(t.TempDir())
	first := p.Fingerprint()
	if p.Fingerprint() != first {
		t.Fatal("unstable fingerprint")
	}
	zero := 0.0
	p.SetRules([]Rule{{Provider: "test", Model: "model", Input: &zero}})
	second := p.Fingerprint()
	if second == first {
		t.Fatal("rule did not change fingerprint")
	}
	p.SetRules([]Rule{{Provider: "test", Model: "model", Input: &zero}})
	if p.Fingerprint() != second {
		t.Fatal("equivalent rule changed fingerprint")
	}
	p.SetAliases(map[[2]string]string{{"test", "model"}: "other"})
	if p.Fingerprint() == second {
		t.Fatal("alias did not change fingerprint")
	}
}

func TestFingerprintTracksRuleStartTime(t *testing.T) {
	p := New(t.TempDir())
	from := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	base := p.Fingerprint()
	p.SetRules([]Rule{{Provider: "test", Multiplier: 0.5}})
	dated := p.Fingerprint()
	if dated == base {
		t.Fatal("rule did not change fingerprint")
	}
	p.SetRules([]Rule{{Provider: "test", Multiplier: 0.5, From: from}})
	moved := p.Fingerprint()
	if moved == dated {
		t.Fatal("rule start time did not change fingerprint")
	}
	// Several rules under one selector are all hashed, and re-adding them in a
	// different order is the same fingerprint.
	p.SetRules([]Rule{
		{Provider: "test", Model: "model", Input: &[]float64{1}[0]},
		{Provider: "test", Model: "model", Input: &[]float64{2}[0], From: from},
	})
	two := p.Fingerprint()
	p.SetRules([]Rule{
		{Provider: "test", Model: "model", Input: &[]float64{2}[0], From: from},
		{Provider: "test", Model: "model", Input: &[]float64{1}[0]},
	})
	if p.Fingerprint() != two {
		t.Fatal("rule order changed fingerprint")
	}
	if p.Fingerprint() == dated {
		t.Fatal("second rule under one selector did not change fingerprint")
	}
}
