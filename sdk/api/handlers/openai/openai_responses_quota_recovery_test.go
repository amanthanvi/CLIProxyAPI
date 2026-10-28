package openai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

func TestResponsesWebsocketQuotaExhaustionRecovery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldLevel := log.GetLevel()
	log.SetLevel(log.PanicLevel)
	t.Cleanup(func() { log.SetLevel(oldLevel) })
	for _, frameKind := range []string{"error", "response.failed"} {
		for _, buffering := range []bool{false, true} {
			for _, scenario := range []string{"early-failover", "exhaust-refill"} {
				t.Run(fmt.Sprintf("%s/buffer=%t/%s", frameKind, buffering, scenario), func(t *testing.T) {
					var exhaustedA, exhaustedB atomic.Bool
					var createsA, createsB atomic.Int32
					exhaustedA.Store(true)
					exhaustedB.Store(scenario == "exhaust-refill")
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						isA := r.Header.Get("Authorization") == "Bearer synthetic-a"
						if !isA && r.Header.Get("Authorization") != "Bearer synthetic-b" {
							http.Error(w, "synthetic unauthorized", 401)
							return
						}
						c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
						if err != nil {
							return
						}
						defer c.Close()
						_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
						for {
							_, payload, err := c.ReadMessage()
							if err != nil {
								return
							}
							if gjson.GetBytes(payload, "type").String() != "response.create" {
								continue
							}
							blocked := exhaustedB.Load()
							label := "b"
							if isA {
								createsA.Add(1)
								blocked = exhaustedA.Load()
								label = "a"
							} else {
								createsB.Add(1)
							}
							if blocked {
								if buffering {
									_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"failed-buffered","status":"in_progress","output":[]}}`))
								}
								quota := `{"type":"error","status":429,"error":{"type":"usage_limit_reached","message":"synthetic quota","resets_in_seconds":3600}}`
								if frameKind == "response.failed" {
									quota = `{"type":"response.failed","response":{"error":{"type":"usage_limit_reached","status_code":429,"message":"synthetic quota","resets_in_seconds":3600}}}`
								}
								if c.WriteMessage(websocket.TextMessage, []byte(quota)) != nil {
									return
								}
								continue
							}
							_ = c.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.created","response":{"id":"success-%s","status":"in_progress","output":[]}}`, label)))
							if c.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"success-%s","status":"completed","output":[]}}`, label))) != nil {
								return
							}
						}
					}))
					t.Cleanup(upstream.Close)
					cfg := &config.Config{RequestRetry: 0, MaxRetryCredentials: 2, Codex: config.CodexConfig{StreamBootstrapBuffering: buffering}}
					manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
					manager.SetConfig(cfg)
					manager.SetRetryConfig(0, 0, 2)
					manager.RegisterExecutor(runtimeexecutor.NewCodexAutoExecutor(cfg))
					model := "primary-handler-" + strings.ReplaceAll(t.Name(), "/", "-")
					ids := []string{"a-" + model, "b-" + model}
					for i, id := range ids {
						key, priority := "synthetic-a", "4"
						if i == 1 {
							key, priority = "synthetic-b", "3"
						}
						_, err := manager.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{ID: id, Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"api_key": key, "base_url": upstream.URL, "websockets": "true", "priority": priority}})
						if err != nil {
							t.Fatal("synthetic auth registration failed")
						}
						registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
						t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
					}
					h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager))
					router := gin.New()
					router.GET("/v1/responses", h.ResponsesWebsocket)
					downstream := httptest.NewServer(router)
					t.Cleanup(downstream.Close)
					dial := func() *websocket.Conn {
						t.Helper()
						c, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(downstream.URL, "http")+"/v1/responses", http.Header{"Session-Id": []string{"stable-primary-handler-session"}})
						if resp != nil && resp.Body != nil {
							_ = resp.Body.Close()
						}
						if err != nil {
							t.Fatal("local downstream dial failed")
						}
						t.Cleanup(func() { _ = c.Close() })
						return c
					}
					request := func(c *websocket.Conn) {
						t.Helper()
						body := fmt.Sprintf(`{"type":"response.create","model":%q,"input":[{"type":"message","role":"user","content":"synthetic full request"}]}`, model)
						if c.WriteMessage(websocket.TextMessage, []byte(body)) != nil {
							t.Fatal("local write failed")
						}
					}
					read := func(c *websocket.Conn) (string, error) {
						_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
						for {
							_, p, err := c.ReadMessage()
							if err != nil {
								return "", err
							}
							if gjson.GetBytes(p, "response.id").String() == "failed-buffered" {
								t.Error("failed account buffered prefix leaked")
							}
							if gjson.GetBytes(p, "type").String() == "response.completed" {
								return gjson.GetBytes(p, "response.id").String(), nil
							}
						}
					}
					first := dial()
					request(first)
					responseID, err := read(first)
					if scenario == "early-failover" {
						if err != nil {
							t.Fatalf("early429 closed downstream before healthy B completion: %v; createsA=%d createsB=%d", err, createsA.Load(), createsB.Load())
						}
						if responseID != "success-b" || createsA.Load() != 1 || createsB.Load() != 1 {
							t.Fatalf("wrong failover result: id=%q A=%d B=%d", responseID, createsA.Load(), createsB.Load())
						}
						// A continuation is account-bound and must ask for full replay, not
						// retry previous_response_id on a different account when B exhausts.
						exhaustedB.Store(true)
						continuation := fmt.Sprintf(`{"type":"response.create","model":%q,"previous_response_id":"success-b","input":[{"type":"message","role":"user","content":"synthetic continuation"}]}`, model)
						if first.WriteMessage(websocket.TextMessage, []byte(continuation)) != nil {
							t.Fatal("continuation write failed")
						}
						_, closeError := read(first)
						var typedClose *websocket.CloseError
						if !errors.As(closeError, &typedClose) || typedClose.Code != websocket.CloseServiceRestart || typedClose.Text != wsHTTPReplayRequiredCloseReason {
							t.Fatalf("continuation did not require full replay: %v", closeError)
						}
						if createsA.Load() != 1 || createsB.Load() != 2 {
							t.Fatalf("continuation rotated account: A=%d B=%d", createsA.Load(), createsB.Load())
						}
						return
					}
					if err == nil {
						t.Fatal("exhausted pool unexpectedly completed")
					}
					if ne, ok := err.(interface{ Timeout() bool }); ok && ne.Timeout() {
						t.Fatal("exhausted request hung until local deadline")
					}
					// All-failed policy closes this socket. Refill/reset is followed by a fresh
					// full request with the SAME session header, never a cross-account continuation.
					_ = first.Close()
					waitPark := func(id string) {
						t.Helper()
						deadline := time.Now().Add(time.Second)
						for {
							a, ok := manager.GetByID(id)
							if ok && a.Quota.Exceeded {
								return
							}
							if time.Now().After(deadline) {
								t.Fatal("synthetic rejected account was not parked")
							}
							runtime.Gosched()
						}
					}
					waitPark(ids[0])
					// The old executor can abort the request before B is attempted. A new full
					// request then drains B, rather than pretending the first request tried both.
					if a, _ := manager.GetByID(ids[1]); !a.Quota.Exceeded {
						drainB := dial()
						request(drainB)
						if _, err := read(drainB); err == nil {
							t.Fatal("second exhausted account unexpectedly completed")
						}
						_ = drainB.Close()
					}
					waitPark(ids[1])
					if createsA.Load() < 1 || createsB.Load() < 1 {
						t.Fatal("both accounts were not actually attempted before refill")
					}
					exhaustedA.Store(false)
					exhaustedB.Store(false)
					for _, id := range ids {
						if _, _, err := manager.ResetQuota(coreauth.WithSkipPersist(context.Background()), id); err != nil {
							t.Fatal("synthetic reset failed")
						}
					}
					second := dial()
					request(second)
					responseID, err = read(second)
					if err != nil || (responseID != "success-a" && responseID != "success-b") {
						t.Fatalf("refill/reset did not recover fresh full request same session: id=%q err=%v", responseID, err)
					}
				})
			}
		}
	}
}
