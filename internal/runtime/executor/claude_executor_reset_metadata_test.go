package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestClaudeExecutorPreservesValidatedResetMetadata(t *testing.T) {
	reset := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.Header().Set("Anthropic-Ratelimit-Unified-Representative-Claim", "five_hour")
		w.Header().Set("Anthropic-Ratelimit-Unified-Reset", reset)
		w.Header().Set("Set-Cookie", "secret")
		w.Header().Set("Authorization", "secret")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"quota reached"}}`))
	}))
	defer upstream.Close()
	exec := NewClaudeExecutor(&config.Config{})
	auth := &coreauth.Auth{ID: "test", Provider: "claude", Attributes: map[string]string{"api_key": "fixture", "base_url": upstream.URL}}
	req := coreexecutor.Request{Model: "claude-opus-5-5", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}
	opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatClaude}
	for _, stream := range []bool{false, true} {
		var err error
		if stream {
			_, err = exec.ExecuteStream(context.Background(), auth, req, opts)
		} else {
			_, err = exec.Execute(context.Background(), auth, req, opts)
		}
		got := coreauth.SafeClaudeRateLimitResponseHeaders(err, time.Now())
		if got.Get("Anthropic-Ratelimit-Unified-Reset") != reset || len(got) != 3 {
			t.Fatalf("stream=%t headers=%v err=%T", stream, got, err)
		}
	}
}
