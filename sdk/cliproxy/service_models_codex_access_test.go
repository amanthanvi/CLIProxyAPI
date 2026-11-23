package cliproxy

import (
	"context"
	"strings"
	"testing"

	"github.com/MoeForge/moego/base64"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func testCodexAccessAuth() *coreauth.Auth {
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"account","chatgpt_plan_type":"pro"}}`))
	return &coreauth.Auth{
		ID: "auth", Index: "index", Provider: "codex",
		Attributes: map[string]string{"auth_kind": "oauth"},
		Metadata:   map[string]any{"id_token": "header." + claims + ".signature", "account_id": "account"},
	}
}

func TestRegisterModelsForAuthCodexPreservesNativeAndAliasProvenance(t *testing.T) {
	const native, excluded, alias = "gpt-6.2-astra", "gpt-6.2-sol", "cpamc-test-alias"
	a := testCodexAccessAuth()
	a.ID, a.Prefix = t.Name(), "codex"
	reg := registry.GetGlobalRegistry()
	t.Cleanup(func() { reg.UnregisterClient(a.ID) })
	service := &Service{cfg: &config.Config{
		SDKConfig: config.SDKConfig{ForceModelPrefix: true},
		OAuthExcludedModels: map[string][]string{
			"codex": {excluded},
		},
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"codex": {{Name: native, Alias: alias, Fork: true}},
		},
	}}
	if !service.registerModelsForAuth(context.Background(), a) {
		t.Fatal("registration was superseded")
	}
	byID := make(map[string]*ModelInfo)
	for _, model := range reg.GetModelsForClient(a.ID) {
		byID[model.ID] = model
	}
	if byID["codex/"+excluded] != nil {
		t.Fatal("excluded plan row was published")
	}
	nativeRow := byID["codex/"+native]
	if nativeRow == nil || !nativeRow.SupportConfigurationUpdate ||
		nativeRow.CodexAccessProgramModelID != native || nativeRow.CodexAccessProgramAccountHash == "" {
		t.Fatal("native plan row lost upstream capabilities or account binding")
	}
	aliasRow := byID["codex/"+alias]
	if aliasRow == nil || aliasRow.OAuthAlias != alias || aliasRow.OAuthAliasSource != native ||
		aliasRow.MetadataModelID != native {
		t.Fatal("published alias lost its upstream route provenance")
	}
	cohort := map[string]struct{}{nativeRow.CodexAccessProgramAccountHash: {}}
	if !reg.CodexServiceTiersAttested(nativeRow.ID, cohort) || reg.CodexServiceTiersAttested(aliasRow.ID, cohort) {
		t.Fatal("service tier attestation must apply only to the canonical native route")
	}
}

func TestCodexAccessBindingRejectsCanonicalAliasCollision(t *testing.T) {
	for _, tc := range []struct {
		name, alias string
		nativeFirst bool
	}{
		{"native before alias", "native-model", true},
		{"alias before native", "native-model", false},
		{"casefold native before alias", "NATIVE-MODEL", true},
		{"casefold alias before native", "NATIVE-MODEL", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			native := &ModelInfo{ID: "native-model"}
			other := &ModelInfo{ID: "different-upstream"}
			models := []*ModelInfo{native, other}
			if !tc.nativeFirst {
				models = []*ModelInfo{other, native}
			}
			bound := bindNativeCodexAccessPrograms(testCodexAccessAuth(), models)
			cfg := &config.Config{OAuthModelAlias: map[string][]config.OAuthModelAlias{
				"codex": {{Name: "different-upstream", Alias: tc.alias, Fork: true}},
			}}
			rows := applyOAuthModelAliasForAuth(cfg, "codex", "oauth", nil, bound)
			found := false
			for _, row := range applyModelPrefixes(rows, "codex", true) {
				if !strings.EqualFold(row.ID, "codex/native-model") {
					continue
				}
				found = true
				if row.CodexAccessProgramModelID == "native-model" {
					t.Fatal("colliding native row retained upstream provenance")
				}
			}
			if !found {
				t.Fatal("missing public native-name row")
			}
			if bound[0].CodexAccessProgramAccountHash == "" || bound[1].CodexAccessProgramAccountHash == "" {
				t.Fatal("alias processing mutated source bindings")
			}
		})
	}
}

func TestCodexAccessBindingUsesOAuthAliasChannelAndPerAuthAliases(t *testing.T) {
	bound := bindNativeCodexAccessPrograms(testCodexAccessAuth(), []*ModelInfo{{ID: "native-model"}})
	cfg := &config.Config{OAuthModelAlias: map[string][]config.OAuthModelAlias{
		"claude": {{Name: "other", Alias: "native-model"}},
	}}
	rows := applyOAuthModelAliasForAuth(cfg, "codex", "oauth", nil, bound)
	if len(rows) != 1 || rows[0].CodexAccessProgramAccountHash == "" {
		t.Fatal("unrelated provider alias revoked native provenance")
	}
	auth := testCodexAccessAuth()
	coreauth.SetOAuthModelAliasesAttribute(auth, []config.OAuthModelAlias{{Name: "other", Alias: "NATIVE-MODEL"}})
	rows = applyOAuthModelAliasForAuth(cfg, "codex", "oauth", auth.Attributes, bound)
	if len(rows) != 1 || rows[0].CodexAccessProgramAccountHash != "" {
		t.Fatal("per-auth alias collision retained native provenance")
	}
	cfg.OAuthModelAlias["codex"] = []config.OAuthModelAlias{{Name: "other", Alias: "native-model"}}
	rows = applyOAuthModelAliasForAuth(cfg, "codex", "oauth", nil, bound)
	if len(rows) != 1 || rows[0].CodexAccessProgramAccountHash != "" {
		t.Fatal("updated Codex alias configuration retained native provenance")
	}
}

func TestBindNativeCodexAccessPrograms(t *testing.T) {
	model := &ModelInfo{ID: "native-model"}
	got := bindNativeCodexAccessPrograms(testCodexAccessAuth(), []*ModelInfo{model})
	if got[0].CodexAccessProgramModelID != model.ID || got[0].CodexAccessProgramAccountHash == "" ||
		model.CodexAccessProgramAccountHash != "" {
		t.Fatal("native binding was missing or mutated shared plan definition")
	}
	if got[0].CodexAccessProgramAccountHash != "5cb7dc289ce1ac00d76445018dd4c79860b2ff55f0c68a3393e2bfc1942c8890" {
		t.Fatal("account hash does not match the publisher wire format")
	}
	vector := testCodexAccessAuth()
	vector.Index = "index-1"
	vector.Metadata["account_id"] = "acct-A"
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"acct-A","chatgpt_plan_type":"pro"}}`))
	vector.Metadata["id_token"] = "header." + claims + ".signature"
	if rows := bindNativeCodexAccessPrograms(vector, []*ModelInfo{model}); rows[0].CodexAccessProgramAccountHash != "c9bb59d5008c3262a3e0cf78c64c1e3e02d0335ee1578d77259b8e020d0647f0" {
		t.Fatal("publisher reference vector did not match")
	}
	prefixed := applyModelPrefixes(got, "codex", true)
	if len(prefixed) != 1 || prefixed[0].ID != "codex/native-model" ||
		prefixed[0].CodexAccessProgramModelID != "native-model" {
		t.Fatal("native prefix did not preserve original binding")
	}
	for _, tc := range []struct {
		name   string
		mutate func(*coreauth.Auth)
	}{
		{"API key", func(a *coreauth.Auth) { a.Attributes["auth_kind"] = "apikey" }},
		{"contradictory API key", func(a *coreauth.Auth) { a.Attributes["api_key"] = "synthetic" }},
		{"SIWC", func(a *coreauth.Auth) { a.Attributes["use_siwc"] = "true" }},
		{"custom endpoint", func(a *coreauth.Auth) { a.Attributes["base_url"] = "https://example.invalid" }},
		{"header override", func(a *coreauth.Auth) { a.Attributes["header:Chatgpt-Account-Id"] = "other" }},
		{"different actual account", func(a *coreauth.Auth) { a.Metadata["account_id"] = "other" }},
		{"missing account", func(a *coreauth.Auth) { delete(a.Metadata, "account_id") }},
		{"missing ID token", func(a *coreauth.Auth) { delete(a.Metadata, "id_token") }},
		{"wrong provider", func(a *coreauth.Auth) { a.Provider = "zen" }},
		{"disabled", func(a *coreauth.Auth) { a.Disabled = true }},
		{"plugin auth", func(a *coreauth.Auth) { coreauth.MarkPluginVirtualAuth(a, "synthetic", 1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := testCodexAccessAuth()
			tc.mutate(auth)
			got := bindNativeCodexAccessPrograms(auth, []*ModelInfo{model})
			if got[0].CodexAccessProgramAccountHash != "" {
				t.Fatal("unqualified credential acquired account binding")
			}
		})
	}
	for _, unqualified := range []*ModelInfo{
		{ID: "namespace/model"}, {ID: "custom-model", UserDefined: true},
		{ID: "compat-model", IsCompat: true},
		{ID: "header-model", Config: &registry.ModelConfig{OverrideHeader: map[string]string{"Authorization": "synthetic"}}},
	} {
		rows := bindNativeCodexAccessPrograms(testCodexAccessAuth(), []*ModelInfo{unqualified})
		if rows[0].CodexAccessProgramAccountHash != "" {
			t.Fatal("unqualified model acquired account binding")
		}
	}
}

func TestNativeCodexIdentityBindingPermitsOnlyPresetPresentationHeaders(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   bool
	}{
		{"User-Agent", true}, {"user-agent", true}, {"originator", true},
		{"Authorization", false}, {"ChatGPT-Account-ID", false}, {"Host", false}, {"X-Custom", false},
	} {
		model := &ModelInfo{ID: "native-model", Config: &registry.ModelConfig{OverrideHeader: map[string]string{tc.header: "synthetic"}}}
		rows := bindNativeCodexAccessPrograms(testCodexAccessAuth(), []*ModelInfo{model})
		if got := rows[0].CodexAccessProgramModelID != ""; got != tc.want {
			t.Fatalf("header %s binding = %t, want %t", tc.header, got, tc.want)
		}
		if model.CodexAccessProgramModelID != "" {
			t.Fatal("mutated shared native model")
		}
	}
}
