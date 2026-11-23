package auth

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// PreservesCodexAccessProgramSelection checks the active request policy at
// catalog time. Conditions and model patterns never waive a potentially
// relevant rewrite: a catalog offer is not scoped to a particular request.
func (m *Manager) PreservesCodexAccessProgramSelection() bool {
	return m.preservesCodexSelection("access_programs")
}

// PreservesCodexServiceTierSelection refuses catalog offers when request policy
// may rewrite the model or the user's selected native service tier.
func (m *Manager) PreservesCodexServiceTierSelection() bool {
	return m.preservesCodexSelection("service_tier")
}

func (m *Manager) preservesCodexSelection(protectedRoot string) bool {
	cfg := m.runtimeConfigSnapshot()
	if cfg == nil {
		return false
	}
	for _, rules := range [][]config.PayloadRule{cfg.Payload.Default, cfg.Payload.DefaultRaw, cfg.Payload.Override, cfg.Payload.OverrideRaw} {
		for _, rule := range rules {
			for path := range rule.Params {
				if !unrelatedCodexSelectionPath(path, protectedRoot) {
					return false
				}
			}
		}
	}
	for _, rule := range cfg.Payload.Filter {
		for _, path := range rule.Params {
			if !unrelatedCodexSelectionPath(path, protectedRoot) {
				return false
			}
		}
	}
	return true
}

// Only ordinary dotted member paths have an unambiguous root. Treat GJSON
// operators, escaping, wildcard selectors, and raw path expressions as unknown.
func unrelatedCodexSelectionPath(path, protectedRoot string) bool {
	for i, part := range strings.Split(path, ".") {
		if part == "" || (i == 0 && (part == "model" || part == protectedRoot)) {
			return false
		}
		for _, c := range part {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
				return false
			}
		}
	}
	return true
}
