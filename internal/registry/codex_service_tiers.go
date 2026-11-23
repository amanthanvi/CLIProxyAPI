package registry

import (
	"strings"

	"github.com/MoeForge/moego/json"
)

// CodexServiceTierMetadata contains only the public native speed descriptors.
// These are observed catalog options, not a request-time authorization grant.
type CodexServiceTierMetadata struct {
	ServiceTiers       []map[string]string `json:"service_tiers"`
	DefaultServiceTier *string             `json:"default_service_tier"`
	AdditionalSpeed    []string            `json:"additional_speed_tiers"`
}

func parseCodexServiceTiers(data []byte) map[string]CodexServiceTierMetadata {
	var payload struct {
		Models []struct {
			Slug  string `json:"slug"`
			Tiers []struct {
				ID          string  `json:"id"`
				Name        string  `json:"name"`
				Description *string `json:"description"`
			} `json:"service_tiers"`
			Default *string  `json:"default_service_tier"`
			Speed   []string `json:"additional_speed_tiers"`
		} `json:"models"`
		ObservedRoutes map[string]struct {
			Cyber []string `json:"cyber"`
		} `json:"codex_access_programs_by_route"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return nil
	}
	result := make(map[string]CodexServiceTierMetadata)
	for _, row := range payload.Models {
		if row.Slug == "" || strings.TrimSpace(row.Slug) != row.Slug || strings.Contains(row.Slug, "/") {
			continue
		}
		// Existing private route keys attest a fresh common model observation,
		// independently of whether that model offers a Cyber Access program.
		observed, ok := payload.ObservedRoutes["codex/"+row.Slug]
		if !ok || observed.Cyber == nil {
			continue
		}
		seen := make(map[string]struct{})
		valid := true
		metadata := CodexServiceTierMetadata{ServiceTiers: make([]map[string]string, 0, len(row.Tiers)), DefaultServiceTier: row.Default, AdditionalSpeed: row.Speed}
		for _, tier := range row.Tiers {
			id := tier.ID
			if id == "" || id == "default" || strings.TrimSpace(id) != id || strings.TrimSpace(tier.Name) == "" {
				valid = false
			}
			if _, duplicate := seen[id]; duplicate {
				valid = false
			}
			seen[id] = struct{}{}
			descriptor := map[string]string{"id": id, "name": tier.Name}
			if tier.Description != nil {
				descriptor["description"] = *tier.Description
			}
			metadata.ServiceTiers = append(metadata.ServiceTiers, descriptor)
		}
		if row.Default != nil {
			if _, present := seen[*row.Default]; !present {
				valid = false
			}
		}
		for _, speed := range row.Speed {
			id := speed
			if speed == "fast" {
				id = "priority"
			}
			if _, present := seen[id]; !present {
				valid = false
			}
		}
		if valid {
			result[row.Slug] = metadata
		}
	}
	return result
}

// AttestedCodexServiceTiers returns a fresh copy only for a native route whose
// serving account identities match the same trusted publisher snapshot.
func (r *ModelRegistry) AttestedCodexServiceTiers(id string) *CodexServiceTierMetadata {
	codexClientCatalogStore.mu.RLock()
	defer codexClientCatalogStore.mu.RUnlock()
	if codexClientCatalogStore.accessProgramsSource != trustedCodexAccessProgramsSource ||
		codexClientCatalogStore.accessProgramsSource != codexClientCatalogStore.accessProgramsSelectedSource ||
		!r.CodexServiceTiersAttested(id, codexClientCatalogStore.accessProgramAccounts) {
		return nil
	}
	row, ok := codexClientCatalogStore.serviceTiersByModel[strings.TrimPrefix(id, "codex/")]
	if !ok {
		return nil
	}
	copy := CodexServiceTierMetadata{ServiceTiers: make([]map[string]string, len(row.ServiceTiers)), AdditionalSpeed: append([]string{}, row.AdditionalSpeed...)}
	for i, tier := range row.ServiceTiers {
		copy.ServiceTiers[i] = make(map[string]string, len(tier))
		for key, value := range tier {
			copy.ServiceTiers[i][key] = value
		}
	}
	if row.DefaultServiceTier != nil {
		value := *row.DefaultServiceTier
		copy.DefaultServiceTier = &value
	}
	return &copy
}
