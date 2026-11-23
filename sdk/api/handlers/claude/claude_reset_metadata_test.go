package claude

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	executors "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestClaudeHTTPErrorPreservesNativeResetWithoutPassthrough(t *testing.T) {
	reset := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.Header().Set("Anthropic-Ratelimit-Unified-Representative-Claim", "five_hour")
		w.Header().Set("Anthropic-Ratelimit-Unified-Reset", reset)
		w.Header().Set("Anthropic-Ratelimit-Unified-Overage-Status", "rejected")
		w.Header().Set("Set-Cookie", "secret")
		w.Header().Set("Authorization", "secret")
		w.Header().Set("X-Private", "secret")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"quota reached"}}`))
	}))
	defer upstream.Close()
	exec := executors.NewClaudeExecutor(&config.Config{})
	_, err := exec.ExecuteStream(context.Background(), &coreauth.Auth{ID: "fixture", Provider: "claude", Attributes: map[string]string{"api_key": "fixture", "base_url": upstream.URL}}, coreexecutor.Request{Model: "claude-opus-5-5", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}, coreexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if err == nil {
		t.Fatal("expected upstream429")
	}
	// Manager/error wrappers must not erase the native reporting metadata.
	err = fmt.Errorf("wrapped: %w", err)
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	h := NewClaudeCodeAPIHandler(&handlers.BaseAPIHandler{})
	h.WriteErrorResponse(c, handlers.ExecutionErrorMessage(err))
	if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Anthropic-Ratelimit-Unified-Reset") != reset || recorder.Header().Get("Anthropic-Ratelimit-Unified-Representative-Claim") != "five_hour" {
		t.Fatalf("status=%d headers=%v", recorder.Code, recorder.Header())
	}
	for _, name := range []string{"Set-Cookie", "Authorization", "X-Private"} {
		if recorder.Header().Get(name) != "" {
			t.Fatalf("unsafe header %s", name)
		}
	}
}
