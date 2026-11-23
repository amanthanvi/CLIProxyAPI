package registry

import (
	"strings"
	"testing"

	"github.com/MoeForge/moego/json"
)

func TestAttestedCodexServiceTiersExactSourceRouteAndCohort(t *testing.T) {
	previous := codexClientCatalogStore
	codexClientCatalogStore = &codexClientModelsStore{}
	t.Cleanup(func() { codexClientCatalogStore = previous })
	const route = "codex/native-model"
	hash := strings.Repeat("a", 64)
	data := []byte(`{"models":[{"slug":"native-model","service_tiers":[{"id":"priority","name":"Fast"},{"id":"ultrafast","name":"Ultrafast"}],"default_service_tier":null,"additional_speed_tiers":["fast","ultrafast"]}],"codex_access_programs_by_route":{"codex/native-model":{"cyber":[]}}}`)
	codexClientCatalogStore.serviceTiersByModel = parseCodexServiceTiers(data)
	codexClientCatalogStore.accessProgramsSource = trustedCodexAccessProgramsSource
	codexClientCatalogStore.accessProgramsSelectedSource = trustedCodexAccessProgramsSource
	codexClientCatalogStore.accessProgramAccounts = map[string]struct{}{hash: {}}
	r := newTestModelRegistry()
	model := &ModelInfo{ID: route, CodexAccessProgramModelID: "native-model", CodexAccessProgramAccountHash: hash,
		Config: &ModelConfig{OverrideHeader: map[string]string{"User-Agent": "native-client", "originator": "codex"}}}
	r.RegisterClient("native", "codex", []*ModelInfo{model})
	got := r.AttestedCodexServiceTiers(route)
	if got == nil || len(got.ServiceTiers) != 2 || got.ServiceTiers[1]["id"] != "ultrafast" || got.DefaultServiceTier != nil {
		t.Fatalf("native offers = %#v", got)
	}
	if r.AttestedCodexCyberPrograms(route) != nil || r.CodexAccessProgramsAttested(route, map[string]struct{}{hash: {}}) {
		t.Fatal("permitting native presentation headers widened Cyber Access attestation")
	}
	got.ServiceTiers[0]["id"] = "changed"
	got.AdditionalSpeed[0] = "changed"
	if fresh := r.AttestedCodexServiceTiers(route); fresh.ServiceTiers[0]["id"] != "priority" || fresh.AdditionalSpeed[0] != "fast" {
		t.Fatal("caller mutated tier metadata")
	}
	for _, id := range []string{"native-model", "zen/native-model", "codex/unknown"} {
		if r.AttestedCodexServiceTiers(id) != nil {
			t.Fatalf("unbound route %s received tiers", id)
		}
	}
	for _, bad := range []*ModelInfo{
		{ID: route},
		{ID: route, CodexAccessProgramModelID: "other", CodexAccessProgramAccountHash: hash},
		{ID: route, CodexAccessProgramModelID: "native-model", CodexAccessProgramAccountHash: strings.Repeat("b", 64)},
		{ID: route, CodexAccessProgramModelID: "native-model", CodexAccessProgramAccountHash: hash,
			Config: &ModelConfig{OverrideHeader: map[string]string{"Authorization": "synthetic"}}},
	} {
		r.RegisterClient("mixed", "codex", []*ModelInfo{bad})
		if r.AttestedCodexServiceTiers(route) != nil {
			t.Fatal("mixed unqualified credential received speed offers")
		}
		r.UnregisterClient("mixed")
	}
	r.RegisterClient("compat", "openai-compatibility", []*ModelInfo{model})
	if r.AttestedCodexServiceTiers(route) != nil {
		t.Fatal("mixed compatibility route received speed offers")
	}
	r.UnregisterClient("compat")
	selectCodexAccessProgramsSource("embed")
	selectCodexAccessProgramsSource(trustedCodexAccessProgramsSource)
	if r.AttestedCodexServiceTiers(route) != nil {
		t.Fatal("source round trip restored stale speed offers")
	}
}

func TestParseCodexServiceTiersRejectsInconsistentMetadata(t *testing.T) {
	for _, metadata := range []string{
		`"service_tiers":[{"id":"priority","name":"Fast"},{"id":"priority","name":"Fast"}]`,
		`"service_tiers":[{"id":"ultrafast","name":"Ultrafast"}],"default_service_tier":"priority"`,
		`"service_tiers":[{"id":"priority","name":"Fast"}],"additional_speed_tiers":["ultrafast"]`,
		`"service_tiers":[{"id":" priority","name":"Fast"}]`,
		`"service_tiers":[{"id":"priority"}]`,
		`"service_tiers":[{"id":"default","name":"Duplicate Standard"}]`,
	} {
		payload := []byte(`{"models":[{"slug":"native-model",` + metadata + `}],"codex_access_programs_by_route":{"codex/native-model":{"cyber":[]}}}`)
		if row, ok := parseCodexServiceTiers(payload)["native-model"]; ok {
			t.Fatalf("accepted inconsistent row: %#v", row)
		}
	}
	// Future native speed IDs remain data, not a version-pinned enum.
	rows := parseCodexServiceTiers([]byte(`{"models":[{"slug":"future","service_tiers":[{"id":"next_tier","name":"Next"}],"additional_speed_tiers":["next_tier"]}],"codex_access_programs_by_route":{"codex/future":{"cyber":[]}}}`))
	if len(rows["future"].ServiceTiers) != 1 {
		t.Fatal("future native speed ID rejected")
	}
	encoded, err := json.Marshal(rows["future"])
	if err != nil || strings.Contains(string(encoded), "Account") {
		t.Fatal("public descriptor encoding contains internal identity")
	}
	unknown := parseCodexServiceTiers([]byte(`{"models":[{"slug":"future","service_tiers":[{"id":"next_tier","name":"Next","future_internal":{"value":"synthetic-private"}}]}],"codex_access_programs_by_route":{"codex/future":{"cyber":[]}}}`))
	encoded, err = json.Marshal(unknown["future"])
	if err != nil || len(unknown["future"].ServiceTiers) != 1 || strings.Contains(string(encoded), "synthetic-private") {
		t.Fatal("unknown descriptor field either invalidated the known tier or leaked publicly")
	}
}

func TestParseCodexServiceTiersRequiresExactFreshObservation(t *testing.T) {
	for _, routes := range []string{`{}`, `{"codex/other":{"cyber":[]}}`, `{"codex/native-model":{}}`, `{"codex/native-model":{"cyber":null}}`} {
		data := []byte(`{"models":[{"slug":"native-model","service_tiers":[{"id":"priority","name":"Fast"}]}],"codex_access_programs_by_route":` + routes + `}`)
		if len(parseCodexServiceTiers(data)) != 0 {
			t.Fatal("unobserved or legacy reusable tier row was accepted")
		}
	}
}
