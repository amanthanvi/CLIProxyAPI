package diff

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func bareModelPrefixConfig(prefix string) *config.Config {
	return &config.Config{SDKConfig: sdkconfig.SDKConfig{ClaudeCode: sdkconfig.ClaudeCodeConfig{BareModelPrefix: prefix}}}
}

func TestBuildConfigChangeDetails_ClaudeCodeBareModelPrefix(t *testing.T) {
	expectContains(t, BuildConfigChangeDetails(bareModelPrefixConfig(""), bareModelPrefixConfig("claude")), `claude-code.bare-model-prefix: "" -> "claude"`)
	expectContains(t, BuildConfigChangeDetails(bareModelPrefixConfig("claude"), bareModelPrefixConfig("zen")), `claude-code.bare-model-prefix: "claude" -> "zen"`)
	expectContains(t, BuildConfigChangeDetails(bareModelPrefixConfig("claude"), bareModelPrefixConfig("")), `claude-code.bare-model-prefix: "claude" -> ""`)
	for _, detail := range BuildConfigChangeDetails(bareModelPrefixConfig("claude"), bareModelPrefixConfig(" claude ")) {
		if strings.HasPrefix(detail, "claude-code.bare-model-prefix") {
			t.Fatalf("whitespace-only prefix change reported: %q", detail)
		}
	}
}
