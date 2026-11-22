package models

import (
	"bytes"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func TestServiceTierTemplatesCannotGrantSpeedOptions(t *testing.T) {
	useCodexClientTemplateSnapshot(t, []byte(`{"models":[{"slug":"native-model","service_tiers":[{"id":"ultrafast","name":"Ultrafast"}],"default_service_tier":"ultrafast","additional_speed_tiers":["ultrafast"]}]}`))
	inputs := []map[string]any{{"id": "codex/native-model"}, {"id": "zen/native-model"}, {"id": "go/native-model"}, {"id": "native-model"}, {"id": "fallback"}}
	for _, pure := range []bool{false, true} {
		rows := buildCodexClientModelsWithToolCapabilities(inputs, func(string) []string {
			if pure {
				return []string{"codex"}
			}
			return []string{"openai-compatibility"}
		}, nil, nil, nil, nil, nil, nil, false, "0.171.0")
		encoded, err := MarshalCompact(rows)
		if err != nil || bytes.Contains(encoded, []byte("ultrafast")) {
			t.Fatal("unproven route inherited template speed tier")
		}
	}
}

func TestServiceTiersPublishedOnlyOnExactAttestedNativeRoute(t *testing.T) {
	useCodexClientTemplateSnapshot(t, []byte(`{"models":[{"slug":"native-model"},{"slug":"not-offered"}]}`))
	inputs := []map[string]any{{"id": "codex/native-model"}, {"id": "codex/not-offered"}, {"id": "zen/native-model"}, {"id": "go/native-model"}, {"id": "native-model"}, {"id": "codex/fallback"}}
	queried := 0
	rows := buildCodexClientModelsWithToolCapabilities(inputs, nil, nil, nil, nil, nil, nil, func(id string) *registry.CodexServiceTierMetadata {
		queried++
		if id != "codex/native-model" {
			return nil
		}
		return &registry.CodexServiceTierMetadata{ServiceTiers: []map[string]string{{"id": "priority", "name": "Fast"}, {"id": "ultrafast", "name": "Ultrafast"}}, AdditionalSpeed: []string{"fast", "ultrafast"}}
	}, false, "0.171.0")
	if queried != 2 {
		t.Fatalf("speed metadata queried for %d routes, want only exact native templates", queried)
	}
	for _, row := range rows {
		encoded, _ := MarshalCompact(row)
		if stringModelValue(row, "slug") == "codex/native-model" {
			if !bytes.Contains(encoded, []byte(`"id":"ultrafast"`)) || !bytes.Contains(encoded, []byte(`"default_service_tier":null`)) {
				t.Fatal("native speed offers or Standard default lost")
			}
		} else if bytes.Contains(encoded, []byte("ultrafast")) {
			t.Fatal("non-native/unknown route received speed metadata")
		}
	}
}
