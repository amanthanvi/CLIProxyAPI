package config

import (
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
