package auth

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestPreservesCodexAccessProgramSelection(t *testing.T) {
	m := &Manager{}
	if m.PreservesCodexAccessProgramSelection() {
		t.Fatal("unknown runtime configuration accepted")
	}
	rule := func(path string) []config.PayloadRule {
		return []config.PayloadRule{{Params: map[string]any{path: nil}}}
	}
	for _, tc := range []struct {
		name string
		cfg  config.PayloadConfig
		want bool
	}{
		{"none", config.PayloadConfig{}, true},
		{"unrelated", config.PayloadConfig{Override: rule("reasoning.effort")}, true},
		{"default", config.PayloadConfig{Default: rule("access_programs")}, false},
		{"default raw", config.PayloadConfig{DefaultRaw: rule("model")}, false},
		{"override", config.PayloadConfig{Override: rule("access_programs.cyber")}, false},
		{"override raw", config.PayloadConfig{OverrideRaw: rule("model")}, false},
		{"filter", config.PayloadConfig{Filter: []config.PayloadFilterRule{{Params: []string{"model"}}}}, false},
		{"unknown expression", config.PayloadConfig{Override: rule("@this")}, false},
		{"ambiguous root", config.PayloadConfig{Override: rule("access_*")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m.runtimeConfig.Store(&config.Config{Payload: tc.cfg})
			if got := m.PreservesCodexAccessProgramSelection(); got != tc.want {
				t.Fatalf("preserves program selection = %t, want %t", got, tc.want)
			}
		})
	}
}
