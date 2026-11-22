package registry

import (
	"encoding/hex"
	"encoding/json"
	"strings"
)

// Deployment owns this private publisher path and its root-controlled directory.
// Arbitrary local catalogs are not entitlement authorities.
const trustedCodexAccessProgramsSource = "/var/lib/cpamc-native-models/codex-client-models.json"

// Private publisher metadata is never taken from shared or remote templates.
func parseCodexAccessPrograms(data []byte, source string) (map[string]struct{}, map[string][]string) {
	if source != trustedCodexAccessProgramsSource {
		return nil, nil
	}
	var payload struct {
		Accounts []string `json:"codex_access_program_account_hashes"`
		Routes   map[string]struct {
			Cyber []string `json:"cyber"`
		} `json:"codex_access_programs_by_route"`
	}
	if json.Unmarshal(data, &payload) != nil || len(payload.Accounts) == 0 {
		return nil, nil
	}
	accounts := make(map[string]struct{}, len(payload.Accounts))
	for _, hash := range payload.Accounts {
		decoded, err := hex.DecodeString(hash)
		if err != nil || len(decoded) != 32 || strings.ToLower(hash) != hash {
			return nil, nil
		}
		if _, duplicate := accounts[hash]; duplicate {
			return nil, nil
		}
		accounts[hash] = struct{}{}
	}
	programs := make(map[string][]string, len(payload.Routes))
	for route, entry := range payload.Routes {
		slug, ok := strings.CutPrefix(route, "codex/")
		if !ok || slug == "" || strings.Contains(slug, "/") || strings.TrimSpace(slug) != slug {
			return nil, nil
		}
		hasStandard := false
		seen := make(map[string]struct{}, len(entry.Cyber))
		for _, program := range entry.Cyber {
			if program == "" || strings.TrimSpace(program) != program {
				return nil, nil
			}
			if _, duplicate := seen[program]; duplicate {
				return nil, nil
			}
			seen[program] = struct{}{}
			hasStandard = hasStandard || program == "standard"
		}
		if hasStandard {
			programs[route] = append([]string(nil), entry.Cyber...)
		}
	}
	return accounts, programs
}

func selectCodexAccessProgramsSource(source string) {
	codexClientCatalogStore.mu.Lock()
	defer codexClientCatalogStore.mu.Unlock()
	if source != codexClientCatalogStore.accessProgramsSelectedSource {
		codexClientCatalogStore.accessProgramAccounts = nil
		codexClientCatalogStore.accessProgramsByRoute = nil
	}
	codexClientCatalogStore.accessProgramsSelectedSource = source
}

// AttestedCodexCyberPrograms resolves only exact publisher routes whose current
// serving credentials are all represented in the first-party discovery cohort.
func (r *ModelRegistry) AttestedCodexCyberPrograms(id string) []string {
	codexClientCatalogStore.mu.RLock()
	defer codexClientCatalogStore.mu.RUnlock()
	if codexClientCatalogStore.accessProgramsSource != trustedCodexAccessProgramsSource ||
		codexClientCatalogStore.accessProgramsSource != codexClientCatalogStore.accessProgramsSelectedSource {
		return nil
	}
	programs := codexClientCatalogStore.accessProgramsByRoute[id]
	if len(programs) == 0 || !r.CodexAccessProgramsAttested(id, codexClientCatalogStore.accessProgramAccounts) {
		return nil
	}
	return append([]string(nil), programs...)
}
