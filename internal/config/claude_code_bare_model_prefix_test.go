package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestParseConfigBytes_ClaudeCodeBareModelPrefix(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte(`claude-code:
  disable-cloaking-model-list: true
  bare-model-prefix: "claude"
`))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}
	if cfg.ClaudeCode.BareModelPrefix != "claude" || !cfg.ClaudeCode.DisableCloakingModelList {
		t.Fatalf("claude-code = %+v, want bare-model-prefix claude with cloaking disabled", cfg.ClaudeCode)
	}

	encoded, errMarshal := yaml.Marshal(cfg.ClaudeCode)
	if errMarshal != nil {
		t.Fatalf("yaml.Marshal() error = %v", errMarshal)
	}
	var decoded ClaudeCodeConfig
	if errUnmarshal := yaml.Unmarshal(encoded, &decoded); errUnmarshal != nil {
		t.Fatalf("yaml.Unmarshal() error = %v", errUnmarshal)
	}
	if decoded != cfg.ClaudeCode {
		t.Fatalf("round trip = %+v, want %+v", decoded, cfg.ClaudeCode)
	}

	unset, errUnset := ParseConfigBytes([]byte(`claude-code:
  disable-cloaking-model-list: true
`))
	if errUnset != nil {
		t.Fatalf("ParseConfigBytes() unset error = %v", errUnset)
	}
	if unset.ClaudeCode.BareModelPrefix != "" {
		t.Fatalf("default bare-model-prefix = %q, want empty", unset.ClaudeCode.BareModelPrefix)
	}
	encodedUnset, errMarshalUnset := yaml.Marshal(unset.ClaudeCode)
	if errMarshalUnset != nil {
		t.Fatalf("yaml.Marshal() unset error = %v", errMarshalUnset)
	}
	if strings.Contains(string(encodedUnset), "bare-model-prefix") {
		t.Fatalf("unset prefix must be omitted when saved: %s", encodedUnset)
	}
}

func TestV8ClaudeCodeBareModelPrefixLoadSave(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"canonical", "upstream:\n  claude:\n    bare-model-prefix: scoped\n", "scoped"},
		{"canonical wins", "claude-code: {bare-model-prefix: legacy}\nupstream: {claude: {bare-model-prefix: scoped}}\n", "scoped"},
		{"canonical empty wins", "claude-code: {bare-model-prefix: legacy}\nupstream: {claude: {bare-model-prefix: ''}}\n", ""},
		{"legacy migration", "claude-code: {bare-model-prefix: legacy}\n", "legacy"},
		{"omitted", "upstream: {claude: {disable-cloaking-model-list: true}}\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "config.yaml")
			raw := "# Preserve scoped Claude routing\nserver: {port: 8317}\n" + tc.raw
			if errWrite := os.WriteFile(file, []byte(raw), 0600); errWrite != nil {
				t.Fatal(errWrite)
			}
			cfg, errLoad := LoadConfig(file)
			if errLoad != nil {
				t.Fatal(errLoad)
			}
			for _, view := range []*Config{cfg, cfg.CloneForRuntime(), cfg.ForAPIKey()} {
				if view.ClaudeCode.BareModelPrefix != tc.want {
					t.Fatalf("loaded prefix = %q, want %q", view.ClaudeCode.BareModelPrefix, tc.want)
				}
			}
			if errSave := SaveConfigPreserveComments(file, cfg, true); errSave != nil {
				t.Fatal(errSave)
			}
			saved, errRead := os.ReadFile(file)
			if errRead != nil {
				t.Fatal(errRead)
			}
			if errValidate := ValidateV8Config(saved); errValidate != nil {
				t.Fatal(errValidate)
			}
			var doc yaml.Node
			if errDecode := yaml.Unmarshal(saved, &doc); errDecode != nil {
				t.Fatal(errDecode)
			}
			prefix := yamlPath(doc.Content[0], "upstream.claude.bare-model-prefix")
			if tc.want == "" {
				if tc.name == "omitted" && prefix != nil {
					t.Fatal("empty prefix was not omitted")
				}
				if prefix != nil && prefix.Value != "" {
					t.Fatal("saved empty override regained a legacy value")
				}
			} else if prefix == nil || prefix.Value != tc.want {
				t.Fatalf("canonical saved prefix differs from %q", tc.want)
			}
			if !strings.Contains(string(saved), "# Preserve scoped Claude routing") {
				t.Fatal("save lost existing comment")
			}
			reloaded, errReload := LoadConfig(file)
			if errReload != nil {
				t.Fatal(errReload)
			}
			if reloaded.ClaudeCode.BareModelPrefix != tc.want {
				t.Fatalf("round-trip prefix = %q, want %q", reloaded.ClaudeCode.BareModelPrefix, tc.want)
			}
		})
	}
}
