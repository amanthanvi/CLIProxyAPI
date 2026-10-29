package openai

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestResponsesInterruptRejectedWhenDuplexDisabled(t *testing.T) {
	raw := []byte(`{"type":"response.interrupt","response_id":"r1","mode":"discard_partial_items"}`)
	lastRequest := []byte(`{"model":"gpt-6-astra","stream":true,"input":[]}`)
	_, _, errMsg := normalizeResponsesWebsocketRequest(raw, lastRequest, []byte("[]"))
	if errMsg == nil || !strings.Contains(errMsg.Error.Error(), "unsupported websocket request type: response.interrupt") {
		t.Fatalf("serial mode must not silently acknowledge or rewrite an interrupt: %#v", errMsg)
	}
}

func TestResponsesInterruptInFlight(t *testing.T) {
	for _, scenario := range []string{"resume", "disconnect"} {
		t.Run(scenario, func(t *testing.T) {
			interrupt := []byte(`{"type":"response.interrupt","response_id":"r1","mode":"discard_partial_items","future_field":"preserve"}`)
			accepted := []byte(`{"type":"response.interrupt.accepted","response_id":"r1","sequence_number":2}`)
			interrupted := []byte(`{"type":"response.output_item.interrupted","response_id":"r1","output_index":0,"sequence_number":3}`)
			upstreamClosed := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer close(upstreamClosed)
				defer func() { _ = c.Close() }()
				_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
				read := func() []byte {
					_, p, errRead := c.ReadMessage()
					if errRead != nil {
						t.Errorf("read upstream frame: %v", errRead)
					}
					return p
				}
				write := func(p []byte) {
					if errWrite := c.WriteMessage(websocket.TextMessage, p); errWrite != nil {
						t.Errorf("write upstream frame: %v", errWrite)
					}
				}
				if p := read(); gjson.GetBytes(p, "type").String() != "response.create" {
					t.Errorf("expected initial create, got %s", p)
					return
				}
				write([]byte(`{"type":"response.created","response":{"id":"r1","output":[]}}`))
				// Upstream cannot finish until the interrupt arrives. This catches
				// serial proxies as well as unsupported-control rejection.
				if p := read(); !bytes.Equal(p, interrupt) {
					t.Errorf("interrupt altered or not forwarded in flight: %s", p)
					return
				}
				write(accepted)
				if scenario == "resume" {
					write(interrupted)
					write([]byte(`{"type":"response.incomplete","response":{"id":"r1","output":[],"incomplete_details":{"reason":"interrupted"}}}`))
					p := read()
					if gjson.GetBytes(p, "type").String() != "response.create" || gjson.GetBytes(p, "previous_response_id").String() != "r1" {
						t.Errorf("continuation did not preserve response parent: %s", p)
						return
					}
					if gjson.GetBytes(p, "model").String() != "interrupt-test-model" {
						t.Errorf("continuation changed model: %s", p)
					}
					write([]byte(`{"type":"response.created","response":{"id":"r2","previous_response_id":"r1","output":[]}}`))
					write([]byte(`{"type":"response.completed","response":{"id":"r2","output":[]}}`))
				}
				if _, _, errRead := c.ReadMessage(); errRead == nil {
					t.Error("unexpected frame after downstream close")
				} else if timeout, ok := errRead.(interface{ Timeout() bool }); ok && timeout.Timeout() {
					t.Error("downstream close did not release upstream socket")
				}
			}))
			defer upstream.Close()

			cfg := &config.Config{}
			cfg.Codex.ResponseSteering = true
			cfg.CodexResponseSteering = true
			manager := coreauth.NewManager(nil, nil, nil)
			manager.SetConfig(cfg)
			manager.RegisterExecutor(runtimeexecutor.NewCodexAutoExecutor(cfg))
			authID, model := "interrupt-"+scenario, "interrupt-test-model"
			if _, err := manager.Register(context.Background(), &coreauth.Auth{
				ID: authID, Provider: "codex", Status: coreauth.StatusActive,
				Attributes: map[string]string{"api_key": "test-key", "base_url": upstream.URL, "websockets": "true"},
			}); err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model}})
			defer registry.GetGlobalRegistry().UnregisterClient(authID)
			handler := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager))
			router := gin.New()
			router.GET("/v1/responses", handler.ResponsesWebsocket)
			downstream := httptest.NewServer(router)
			defer downstream.Close()
			c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(downstream.URL, "http")+"/v1/responses", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			send := func(p []byte) {
				t.Helper()
				if errWrite := c.WriteMessage(websocket.TextMessage, p); errWrite != nil {
					t.Fatal(errWrite)
				}
			}
			read := func(kind string) []byte {
				t.Helper()
				_, p, errRead := c.ReadMessage()
				if errRead != nil {
					t.Fatal(errRead)
				}
				if gjson.GetBytes(p, "type").String() != kind {
					t.Fatalf("expected %s, got %s", kind, p)
				}
				return p
			}
			send([]byte(fmt.Sprintf(`{"type":"response.create","model":%q,"input":[]}`, model)))
			read("response.created")
			// Unsupported control frames still fail locally without touching upstream.
			send([]byte(`{"type":"response.unrecognized"}`))
			if p := read("error"); !strings.Contains(gjson.GetBytes(p, "error.message").String(), "unsupported websocket request type: response.unrecognized") {
				t.Fatalf("unsupported-type guard changed: %s", p)
			}
			send(interrupt)
			if p := read("response.interrupt.accepted"); !bytes.Equal(p, accepted) {
				t.Fatalf("interrupt acknowledgment altered: %s", p)
			}
			if scenario == "resume" {
				if p := read("response.output_item.interrupted"); !bytes.Equal(p, interrupted) {
					t.Fatalf("partial-item interruption altered: %s", p)
				}
				read("response.incomplete")
				send([]byte(fmt.Sprintf(`{"type":"response.create","model":%q,"previous_response_id":"r1","input":[{"role":"user","content":[{"type":"input_text","text":"continue"}]}]}`, model)))
				read("response.created")
				read("response.completed")
			}
			_ = c.Close()
			select {
			case <-upstreamClosed:
			case <-time.After(2 * time.Second):
				t.Fatal("upstream socket remained open after downstream cancellation")
			}
		})
	}
}
