package openai

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestResponsesWebsocketParkedPoolUsesNativeTerminalQuota(t *testing.T) {
	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	manager.SetConfig(&config.Config{})
	manager.SetRetryConfig(0, 0, 0)
	executor := &responsesPoolQuotaExecutor{}
	manager.RegisterExecutor(executor)
	model := "parked-pool-native-websocket"
	ids := []string{"parked-pool-ws-a", "parked-pool-ws-b"}
	for _, id := range ids {
		_, err := manager.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{ID: id, Provider: "codex", Status: coreauth.StatusActive})
		if err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		delay := 451594 * time.Second
		manager.MarkResult(context.Background(), coreauth.Result{AuthID: id, Provider: "codex", Model: model,
			CredentialScope: true, RetryAfter: &delay, Error: &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "synthetic quota"}})
	}
	_, err := manager.ExecuteStream(context.Background(), []string{"codex"}, coreexecutor.Request{Model: model}, coreexecutor.Options{})
	if _, certified := coreauth.PoolQuotaExhaustionDetails(err); !certified {
		t.Fatal("fixture did not reach manager-certified full-pool exhaustion")
	}
	payload, buildErr := buildResponsesWebsocketErrorPayload(&interfaces.ErrorMessage{StatusCode: http.StatusTooManyRequests, Error: err})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	t.Logf("synthetic_wire=%s", payload)
	if got := ojson.GetBytes(payload, "error.type").String(); got != "usage_limit_reached" {
		t.Fatalf("certified parked pool emitted error.type=%q; native Codex sleeps the Retry-After deadline instead of terminating", got)
	}
	if ojson.GetBytes(payload, "error.code").String() != "model_cooldown" || ojson.GetBytes(payload, "error.retryable").Raw != "false" {
		t.Fatal("native quota code/retryability contract changed")
	}
	if ojson.GetBytes(payload, "error.resets_at").Exists() || ojson.GetBytes(payload, "error.resets_in_seconds").Exists() {
		t.Fatal("local admission deadline was exposed as provider reset")
	}
	if executor.calls.Load() != 0 {
		t.Fatal("already parked pool made an upstream call")
	}

	// Resuming one credential makes this a pinned-only failure, not a full pool failure.
	if _, _, errReset := manager.ResetQuota(context.Background(), ids[1]); errReset != nil {
		t.Fatal(errReset)
	}
	_, err = manager.ExecuteStream(context.Background(), []string{"codex"}, coreexecutor.Request{Model: model},
		coreexecutor.Options{Metadata: map[string]any{coreexecutor.PinnedAuthMetadataKey: ids[0]}})
	if err == nil {
		t.Fatal("the pinned credential should still be cooling")
	}
	if _, certified := coreauth.PoolQuotaExhaustionDetails(err); certified {
		t.Fatal("pinned credential with a viable alternative was certified as pool exhaustion")
	}
	payload, buildErr = buildResponsesWebsocketErrorPayload(handlers.ExecutionErrorMessage(err))
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	if ojson.GetBytes(payload, "error.type").Exists() || ojson.GetBytes(payload, "error.code").String() != "model_cooldown" ||
		ojson.GetBytes(payload, "status").Int() != http.StatusTooManyRequests || ojson.GetBytes(payload, "headers.Retry-After").String() == "" {
		t.Fatalf("pinned cooldown lost its existing 429 contract: %s", payload)
	}
	if executor.calls.Load() != 0 {
		t.Fatal("a pinned call must not use the ready alternative in place")
	}
}
