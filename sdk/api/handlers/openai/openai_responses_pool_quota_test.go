package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

type responsesPoolQuotaExecutor struct {
	calls     atomic.Int32
	failQuota bool
}

type responsesSyntheticQuotaError struct{}

func (responsesSyntheticQuotaError) Error() string {
	return `{"error":{"type":"usage_limit_reached","message":"synthetic quota","resets_in_seconds":451594}}`
}
func (responsesSyntheticQuotaError) StatusCode() int { return http.StatusTooManyRequests }
func (responsesSyntheticQuotaError) RetryAfter() *time.Duration {
	delay := 451594 * time.Second
	return &delay
}

func (*responsesPoolQuotaExecutor) Identifier() string { return "codex" }
func (e *responsesPoolQuotaExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	e.calls.Add(1)
	if e.failQuota {
		return coreexecutor.Response{}, responsesSyntheticQuotaError{}
	}
	return coreexecutor.Response{}, fmt.Errorf("unexpected synthetic execution")
}
func (e *responsesPoolQuotaExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.calls.Add(1)
	if e.failQuota {
		return nil, responsesSyntheticQuotaError{}
	}
	return nil, fmt.Errorf("unexpected synthetic streaming execution")
}
func (*responsesPoolQuotaExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}
func (*responsesPoolQuotaExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, fmt.Errorf("unexpected token count")
}
func (*responsesPoolQuotaExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("unexpected HTTP request")
}

func TestResponsesInitialExhaustedPoolUsesNativeTerminalQuota(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, alreadyParked := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("parked=%t/stream=%t", alreadyParked, stream), func(t *testing.T) {
				model := "quota-terminal-" + strings.ReplaceAll(t.Name(), "/", "-")
				executor := &responsesPoolQuotaExecutor{failQuota: !alreadyParked}
				selector := coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
					Fallback: &coreauth.RoundRobinSelector{}, TTL: 24 * time.Hour, RebindOn: coreauth.SessionAffinityRebindOnQuota,
				})
				manager := coreauth.NewManager(nil, selector, nil)
				cfg := &config.Config{}
				manager.SetConfig(cfg)
				manager.SetRetryConfig(0, 0, 0)
				manager.RegisterExecutor(executor)
				for index := 0; index < 2; index++ {
					id := fmt.Sprintf("quota-terminal-%d-%s", index, model)
					if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{ID: id, Provider: "codex", Status: coreauth.StatusActive}); err != nil {
						t.Fatal(err)
					}
					registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
					if alreadyParked {
						delay := 451594 * time.Second
						manager.MarkResult(context.Background(), coreauth.Result{AuthID: id, Provider: "codex", Model: model,
							CredentialScope: true, RetryAfter: &delay, Error: &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "synthetic quota"}})
					}
				}
				h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager))
				router := gin.New()
				router.POST("/v1/responses", h.Responses)
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":%q,"input":"synthetic","stream":%t}`, model, stream)))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("User-Agent", "codex_cli_rs/0.165.0")
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, req)
				body := recorder.Body.Bytes()
				if recorder.Code != http.StatusTooManyRequests || gjson.GetBytes(body, "error.type").String() != "usage_limit_reached" {
					t.Fatalf("expected native terminal quota HTTP429, status=%d type=%q code=%q", recorder.Code,
						gjson.GetBytes(body, "error.type").String(), gjson.GetBytes(body, "error.code").String())
				}
				if gjson.GetBytes(body, "error.code").String() != "model_cooldown" {
					t.Fatal("lost compatible model_cooldown code")
				}
				if gjson.GetBytes(body, "error.resets_at").Exists() || gjson.GetBytes(body, "error.resets_in_seconds").Exists() {
					t.Fatal("scheduler admission time must not masquerade as provider reset")
				}
				if gjson.GetBytes(body, "error.admission_retry_at").Exists() || gjson.GetBytes(body, "error.eligible_credentials").Exists() {
					t.Fatal("internal pool diagnostics must not become a native reset contract")
				}
				wantCalls := int32(2)
				if alreadyParked {
					wantCalls = 0
				}
				if executor.calls.Load() != wantCalls {
					t.Fatalf("upstream calls=%d, want%d", executor.calls.Load(), wantCalls)
				}

				// Once SSE is committed, preserve the existing failure event rather than
				// pretending that this request can become a fresh HTTP quota response.
				_, poolErr := manager.Execute(context.Background(), []string{"codex"}, coreexecutor.Request{Model: model}, coreexecutor.Options{})
				committed := httptest.NewRecorder()
				committedContext, _ := gin.CreateTestContext(committed)
				committedContext.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				committedContext.Request.Header.Set("User-Agent", "codex_cli_rs/0.165.0")
				committedContext.Writer.Header().Set("Content-Type", "text/event-stream")
				committedContext.Writer.WriteHeader(http.StatusOK)
				_, _ = committedContext.Writer.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\n"))
				committedContext.Writer.Flush()
				errs := make(chan *interfaces.ErrorMessage, 1)
				errs <- &interfaces.ErrorMessage{StatusCode: http.StatusTooManyRequests, Error: poolErr}
				close(errs)
				data := make(chan []byte)
				close(data)
				h.forwardResponsesStream(committedContext, committedContext.Writer, func(error) {}, data, errs,
					&responsesSSEFramer{failureEvent: "response.failed", isCodexClient: true})
				if committed.Code != http.StatusOK || !strings.Contains(committed.Body.String(), "event: response.failed") ||
					strings.Contains(committed.Body.String(), "All eligible credentials for the requested model are quota limited.") {
					t.Fatal("committed SSE contract changed to an initial HTTP quota response")
				}
			})
		}
	}
}

func TestResponsesInitialQuotaDoesNotTrustCooldownBodyOrMarker(t *testing.T) {
	h, recorder, c, _ := newResponsesStreamTestHandler(t)
	// This real selector-level marker has no manager-owned full-pool certificate.
	quota := coreauth.NewModelCooldownError("synthetic", "codex", 451594*time.Second)
	h.writeInitialResponsesError(c, &interfaces.ErrorMessage{StatusCode: http.StatusTooManyRequests, Error: quota})
	if gjson.GetBytes(recorder.Body.Bytes(), "error.type").String() == "usage_limit_reached" {
		t.Fatal("selector cooldown marker was promoted to authoritative pool exhaustion")
	}
}

func TestResponsesInitialQuotaPreservesDirectResponse(t *testing.T) {
	h, recorder, c, _ := newResponsesStreamTestHandler(t)
	body := []byte(`{"error":{"message":"synthetic direct response","code":"model_cooldown"}}`)
	h.writeInitialResponsesError(c, &interfaces.ErrorMessage{StatusCode: http.StatusTooManyRequests,
		DirectResponse: true, Body: body, Headers: http.Header{"Retry-After": []string{"451594"}}})
	if recorder.Code != http.StatusTooManyRequests || recorder.Body.String() != string(body) || recorder.Header().Get("Retry-After") != "451594" {
		t.Fatal("direct response was rewritten")
	}
}
