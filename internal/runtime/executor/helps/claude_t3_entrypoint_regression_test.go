package helps

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestDetectClaudeCodeRequestConfiguredT3Code323(t *testing.T) {
	var defaults config.ClaudeHeaderDefaults
	if err := json.Unmarshal([]byte(`{"native-entrypoints":["t3-code"]}`), &defaults); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ClaudeHeaderDefaults: defaults}
	headers := confirmedClaudeCodeHeaders()
	headers.Set("User-Agent", "claude-cli/2.1.323 (external, t3-code)")
	for _, countTokens := range []bool{false, true} {
		payload := claudeCodeDetectionPayload(validClaudeCodeMetadataUserID)
		if countTokens {
			payload = []byte(`{"messages":[]}`)
		}
		detection := DetectClaudeCodeRequest(headers, payload, countTokens, cfg)
		if !detection.Confirmed || !detection.StrongSignals || !detection.NativeClient {
			t.Errorf("count_tokens=%v: detection = %+v, want configured T3 Code confirmed", countTokens, detection)
		}
	}
}
