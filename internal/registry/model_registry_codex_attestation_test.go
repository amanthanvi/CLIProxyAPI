package registry

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCodexAccessProgramsAttested(t *testing.T) {
	const id = "codex/native-model"
	bound := &ModelInfo{ID: id, CodexAccessProgramModelID: "native-model", CodexAccessProgramAccountHash: "account-a"}
	cohort := map[string]struct{}{"account-a": {}, "account-b": {}}
	reg := newTestModelRegistry()
	if reg.CodexAccessProgramsAttested(id, cohort) {
		t.Fatal("unregistered route was attested")
	}
	reg.RegisterClient("a", "codex", []*ModelInfo{bound})
	if !reg.CodexAccessProgramsAttested(id, cohort) {
		t.Fatal("exact native route was not attested")
	}
	if reg.CodexAccessProgramsAttested("native-model", cohort) || reg.CodexAccessProgramsAttested("zen/native-model", cohort) ||
		reg.CodexAccessProgramsAttested(id, nil) {
		t.Fatal("bare, compatibility or unknown cohort was attested")
	}
	for _, tc := range []struct {
		name, provider string
		info           *ModelInfo
	}{
		{"unknown credential", "codex", &ModelInfo{ID: id}},
		{"unobserved account", "codex", &ModelInfo{ID: id, CodexAccessProgramModelID: "native-model", CodexAccessProgramAccountHash: "other"}},
		{"aliased model", "codex", &ModelInfo{ID: id, CodexAccessProgramModelID: "different", CodexAccessProgramAccountHash: "account-b"}},
		{"compatibility provider", "openai-compatibility", bound},
		{"header override", "codex", &ModelInfo{ID: id, CodexAccessProgramModelID: "native-model", CodexAccessProgramAccountHash: "account-b", Config: &ModelConfig{OverrideHeader: map[string]string{"Authorization": "synthetic"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg.RegisterClient("b", tc.provider, []*ModelInfo{tc.info})
			if reg.CodexAccessProgramsAttested(id, cohort) {
				t.Fatal("mixed route was attested")
			}
			reg.UnregisterClient("b")
			if !reg.CodexAccessProgramsAttested(id, cohort) {
				t.Fatal("retiring unqualified route did not restore native attestation")
			}
		})
	}
	b := *bound
	b.CodexAccessProgramAccountHash = "account-b"
	reg.RegisterClient("b", "codex", []*ModelInfo{&b})
	if !reg.CodexAccessProgramsAttested(id, cohort) {
		t.Fatal("two observed native credentials were not attested")
	}
	encoded, err := json.Marshal(bound)
	if err != nil || strings.Contains(string(encoded), "account-a") || strings.Contains(string(encoded), "CodexAccess") {
		t.Fatal("internal binding was exposed in model serialization")
	}
}
