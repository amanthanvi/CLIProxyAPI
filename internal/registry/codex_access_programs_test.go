package registry

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestParseCodexAccessProgramsRequiresLocalSourceAndStandard(t *testing.T) {
	source := trustedCodexAccessProgramsSource
	hash := strings.Repeat("a", 64)
	payload := map[string]any{
		"codex_access_program_account_hashes": []string{hash},
		"codex_access_programs_by_route": map[string]any{
			"codex/listed":      map[string]any{"cyber": []string{"standard", "daybreak_blue"}},
			"codex/no-standard": map[string]any{"cyber": []string{"daybreak_blue"}},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, untrusted := range []string{"", "embed", "catalog", "relative.json", "https://example.invalid/catalog.json", `\\server\share\catalog.json`, `\\?\C:\catalog.json`, "C:\\catalog.json", "/tmp/catalog.json"} {
		if accounts, routes := parseCodexAccessPrograms(raw, untrusted); accounts != nil || routes != nil {
			t.Errorf("source %q accepted", untrusted)
		}
	}
	accounts, routes := parseCodexAccessPrograms(raw, source)
	if _, ok := accounts[hash]; !ok {
		t.Fatal("missing account fingerprint")
	}
	if got, want := routes["codex/listed"], []string{"standard", "daybreak_blue"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("programs = %v, want %v", got, want)
	}
	if _, ok := routes["codex/no-standard"]; ok {
		t.Fatal("accepted nonstandard-only programs")
	}
	for _, malformed := range []string{
		`{"codex_access_program_account_hashes":["bad"],"codex_access_programs_by_route":{}}`,
		`{"codex_access_program_account_hashes":["` + hash + `","` + hash + `"],"codex_access_programs_by_route":{}}`,
		`{"codex_access_program_account_hashes":["` + hash + `"],"codex_access_programs_by_route":{"codex/a/b":{"cyber":["standard"]}}}`,
		`{"codex_access_program_account_hashes":["` + hash + `"],"codex_access_programs_by_route":{"codex/a":{"cyber":["standard","standard"]}}}`,
		`{"codex_access_program_account_hashes":["` + hash + `"],"codex_access_programs_by_route":{"codex/a":{"cyber":"standard"}}}`,
	} {
		if accounts, routes := parseCodexAccessPrograms([]byte(malformed), source); accounts != nil || routes != nil {
			t.Fatal("accepted malformed attestation")
		}
	}
}

func TestCodexAccessProgramsSourceSelectionRevokesPriorPublication(t *testing.T) {
	codexClientCatalogStore.mu.Lock()
	previousData := codexClientCatalogStore.data
	previousRevision := codexClientCatalogStore.revision
	previousResponsesLite := codexClientCatalogStore.responsesLiteMap
	previousThinking := codexClientCatalogStore.thinkingLevelsMap
	previousSource := codexClientCatalogStore.accessProgramsSource
	previousSelected := codexClientCatalogStore.accessProgramsSelectedSource
	previousAccounts := codexClientCatalogStore.accessProgramAccounts
	previousPrograms := codexClientCatalogStore.accessProgramsByRoute
	codexClientCatalogStore.mu.Unlock()
	t.Cleanup(func() {
		codexClientCatalogStore.mu.Lock()
		codexClientCatalogStore.data = previousData
		codexClientCatalogStore.revision = previousRevision
		codexClientCatalogStore.responsesLiteMap = previousResponsesLite
		codexClientCatalogStore.thinkingLevelsMap = previousThinking
		codexClientCatalogStore.accessProgramsSource = previousSource
		codexClientCatalogStore.accessProgramsSelectedSource = previousSelected
		codexClientCatalogStore.accessProgramAccounts = previousAccounts
		codexClientCatalogStore.accessProgramsByRoute = previousPrograms
		codexClientCatalogStore.mu.Unlock()
	})
	source := trustedCodexAccessProgramsSource
	var payload map[string]any
	if err := json.Unmarshal(embeddedCodexClientModelsJSON, &payload); err != nil {
		t.Fatal(err)
	}
	payload["codex_access_program_account_hashes"] = []string{strings.Repeat("b", 64)}
	payload["codex_access_programs_by_route"] = map[string]any{
		"codex/listed": map[string]any{"cyber": []string{"standard", "daybreak_blue"}},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	models := newTestModelRegistry()
	models.RegisterClient("local-test-account", "codex", []*ModelInfo{{
		ID:                            "codex/listed",
		CodexAccessProgramModelID:     "listed",
		CodexAccessProgramAccountHash: strings.Repeat("b", 64),
	}})
	selectCodexAccessProgramsSource(source)
	if _, err := codexCatalogUpdater.publishCatalog(raw, source, source); err != nil {
		t.Fatal(err)
	}
	codexClientCatalogStore.mu.RLock()
	published := len(codexClientCatalogStore.accessProgramsByRoute)
	codexClientCatalogStore.mu.RUnlock()
	if published != 1 {
		t.Fatal("explicit local publication did not preserve private metadata")
	}
	got := models.AttestedCodexCyberPrograms("codex/listed")
	if !reflect.DeepEqual(got, []string{"standard", "daybreak_blue"}) {
		t.Fatalf("bound programs = %v", got)
	}
	got[0] = "mutated"
	if models.AttestedCodexCyberPrograms("codex/listed")[0] != "standard" {
		t.Fatal("lookup returned mutable backing slice")
	}
	for _, id := range []string{"listed", "zen/listed", "codex/unknown"} {
		if len(models.AttestedCodexCyberPrograms(id)) != 0 {
			t.Errorf("unbound route %q accepted", id)
		}
	}
	selectCodexAccessProgramsSource("https://example.invalid/catalog.json")
	selectCodexAccessProgramsSource(source)
	codexClientCatalogStore.mu.RLock()
	remaining := len(codexClientCatalogStore.accessProgramsByRoute)
	codexClientCatalogStore.mu.RUnlock()
	if remaining != 0 {
		t.Fatal("source round trip re-enabled old grants")
	}
	if _, err := codexCatalogUpdater.publishCatalog(raw, source, "https://example.invalid/catalog.json"); err != nil {
		t.Fatal(err)
	}
	codexClientCatalogStore.mu.RLock()
	remaining = len(codexClientCatalogStore.accessProgramsByRoute)
	codexClientCatalogStore.mu.RUnlock()
	if remaining != 0 {
		t.Fatal("fallback publication enabled grants")
	}
}
