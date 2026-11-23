package openai

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type pinnedQuotaBeforeAuth struct {
	fn func(pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse
}

func (h pinnedQuotaBeforeAuth) InterceptRequestBeforeAuth(_ context.Context, req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	return h.fn(req)
}
func (pinnedQuotaBeforeAuth) InterceptRequestAfterAuth(context.Context, pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	return pluginapi.RequestInterceptResponse{}
}
func (pinnedQuotaBeforeAuth) InterceptResponse(context.Context, pluginapi.ResponseInterceptRequest) pluginapi.ResponseInterceptResponse {
	return pluginapi.ResponseInterceptResponse{}
}
func (pinnedQuotaBeforeAuth) InterceptStreamChunk(context.Context, pluginapi.StreamChunkInterceptRequest) pluginapi.StreamChunkInterceptResponse {
	return pluginapi.StreamChunkInterceptResponse{}
}
func (pinnedQuotaBeforeAuth) HasStreamInterceptors() bool { return false }

func TestResponsesWebsocketPinParkedBeforeSelection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, scenario := range []string{"ready-alternate", "pool-parked", "interceptor-429"} {
		t.Run(scenario, func(t *testing.T) {
			f := newLossFixture(t, false, func(index int, conn *websocket.Conn, read func() []byte) {
				for p := read(); p != nil; p = read() {
					if ojson.GetBytes(p, "previous_response_id").Exists() {
						t.Error("pinned delta reached an upstream credential")
					}
					if index == 2 && (ojson.GetBytes(p, "model").String() != "loss-model" ||
						ojson.GetBytes(p, "input.#").Int() != 3 ||
						ojson.GetBytes(p, "input.1.type").String() != "function_call_output" ||
						ojson.GetBytes(p, "input.1.call_id").String() != "synthetic-call" ||
						ojson.GetBytes(p, "input.1.output").String() != "once") {
						t.Errorf("full-context retry did not preserve model and tool result: %s", p)
					}
					_ = conn.WriteMessage(websocket.TextMessage, []byte(lossCreated))
					_ = conn.WriteMessage(websocket.TextMessage, []byte(lossCompleted))
				}
			})
			manager := f.base.AuthManager
			var parkCalls atomic.Int32
			f.base.SetPluginHost(pinnedQuotaBeforeAuth{fn: func(req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
				if ojson.GetBytes(req.Body, "previous_response_id").String() != "r2" {
					return pluginapi.RequestInterceptResponse{}
				}
				parkCalls.Add(1)
				pin, _ := req.Metadata[coreexecutor.PinnedAuthMetadataKey].(string)
				if pin == "" {
					t.Error("fixture did not reach the pinned pre-selection boundary")
				}
				if scenario == "interceptor-429" {
					return pluginapi.RequestInterceptResponse{Terminate: true, StatusCode: http.StatusTooManyRequests,
						ResponseBody: []byte(`{"error":{"code":"model_cooldown","message":"synthetic request rejection"}}`)}
				}
				for _, auth := range manager.List() {
					if auth.ID == pin || scenario == "pool-parked" {
						delay := 451594 * time.Second
						manager.MarkResult(context.Background(), coreauth.Result{AuthID: auth.ID, Provider: "codex", Model: "loss-model",
							CredentialScope: true, RetryAfter: &delay, Error: &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "synthetic quota"}})
					}
				}
				return pluginapi.RequestInterceptResponse{}
			}})
			f.create("")
			f.awaitCompleted()
			f.create(`,"previous_response_id":"r2"`)
			_ = f.client.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, frame, errRead := f.client.ReadMessage()
			if scenario == "interceptor-429" {
				if errRead != nil || ojson.GetBytes(frame, "type").String() != "error" ||
					ojson.GetBytes(frame, "status").Int() != http.StatusTooManyRequests {
					t.Fatalf("expected preserved terminal 429, got frame=%s err=%v", frame, errRead)
				}
				if _, _, errAfter := f.client.ReadMessage(); errAfter == nil {
					t.Fatal("terminal 429 did not close the socket")
				}
			} else {
				var closeErr *websocket.CloseError
				if !errors.As(errRead, &closeErr) || closeErr.Code != websocket.CloseServiceRestart || closeErr.Text != wsHTTPReplayRequiredCloseReason {
					t.Fatalf("expected full-context replay close, got frame=%s err=%v", frame, errRead)
				}
			}
			if parkCalls.Load() != 1 || f.creates.Load() != 1 || f.connections.Load() != 1 {
				t.Fatalf("pinned turn made an upstream attempt: before-auth=%d creates=%d connections=%d",
					parkCalls.Load(), f.creates.Load(), f.connections.Load())
			}
			if scenario == "interceptor-429" {
				return
			}
			conn, resp, errDial := websocket.DefaultDialer.Dial(f.wsURL, nil)
			if resp != nil && resp.Body != nil {
				defer func() { _ = resp.Body.Close() }()
			}
			if errDial != nil {
				t.Fatal(errDial)
			}
			defer func() { _ = conn.Close() }()
			full := `{"type":"response.create","model":"loss-model","input":[{"type":"function_call","name":"synthetic","call_id":"synthetic-call","arguments":"{}"},{"type":"function_call_output","call_id":"synthetic-call","output":"once"},{"type":"message","role":"user","content":"continue"}]}`
			if err := conn.WriteMessage(websocket.TextMessage, []byte(full)); err != nil {
				t.Fatal(err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			if scenario == "pool-parked" {
				_, frame, err := conn.ReadMessage()
				if err != nil || ojson.GetBytes(frame, "type").String() != "error" ||
					ojson.GetBytes(frame, "status").Int() != http.StatusTooManyRequests ||
					ojson.GetBytes(frame, "error.type").String() != "usage_limit_reached" {
					t.Fatalf("unreachable pool did not terminate after one full replay: frame=%s err=%v", frame, err)
				}
				if _, _, errAfter := conn.ReadMessage(); errAfter == nil || websocket.IsCloseError(errAfter, websocket.CloseServiceRestart) {
					t.Fatalf("full replay requested another replay instead of closing: %v", errAfter)
				}
				if f.creates.Load() != 1 || f.connections.Load() != 1 {
					t.Fatalf("exhausted pool accepted an upstream request: creates=%d connections=%d", f.creates.Load(), f.connections.Load())
				}
				return
			}
			for {
				_, frame, err := conn.ReadMessage()
				if err != nil || ojson.GetBytes(frame, "type").String() == "error" {
					t.Fatalf("fresh explicit replay failed: frame=%s err=%v", frame, err)
				}
				if ojson.GetBytes(frame, "type").String() == "response.completed" {
					break
				}
			}
			first, okFirst := f.authIDs.Load(1)
			second, okSecond := f.authIDs.Load(2)
			if !okFirst || !okSecond || !strings.HasPrefix(first.(string), "Bearer key-") || first == second || f.creates.Load() != 2 {
				t.Fatalf("full replay did not execute exactly once on the other credential: creates=%d connections=%d", f.creates.Load(), f.connections.Load())
			}
		})
	}
}
