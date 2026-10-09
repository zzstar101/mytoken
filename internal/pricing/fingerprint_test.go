package pricing

import "testing"

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
