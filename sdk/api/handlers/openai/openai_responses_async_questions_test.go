package openai

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	"github.com/tidwall/gjson"
)

// T3's message-mode questions become normal user input: an active answer can
// steer the turn, while an answer after completion starts the next response.
func TestResponsesNativeAsyncQuestionAnswerTransport(t *testing.T) {
	const question = `{"id":"msg_async_question","type":"message","role":"assistant","status":"completed","delivery":"async","phase":"commentary","questions":[{"title":"Preferred color?","options":["Green","Red"]}],"content":[{"type":"output_text","text":"Preferred color?"}]}`
	const answerInput = `[{"type":"message","role":"user","content":[{"type":"input_text","text":"Preferred color?\nGreen"}]}]`
	for _, scenario := range []string{"active_interrupt", "active_interrupt_duplex", "active_steer", "completed_answer"} {
		t.Run(scenario, func(t *testing.T) {
			var connections atomic.Int32
			upstreamDone := make(chan struct{})
			var upstreamDoneOnce sync.Once
			interrupt := []byte(`{"type":"response.interrupt","response_id":"r1","mode":"discard_partial_items"}`)
			steer := []byte(`{"type":"response.steer","previous_response_id":"r1","input":` + answerInput + `}`)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				connections.Add(1)
				defer upstreamDoneOnce.Do(func() { close(upstreamDone) })
				c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer func() { _ = c.Close() }()
				_ = c.SetReadDeadline(time.Now().Add(8 * time.Second))
				read := func() []byte {
					_, payload, err := c.ReadMessage()
					if err != nil {
						t.Errorf("upstream read: %v", err)
					}
					return payload
				}
				write := func(payload string) {
					if err := c.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
						t.Errorf("upstream write: %v", err)
					}
				}
				if gjson.GetBytes(read(), "type").String() != "response.create" {
					t.Error("missing initial response.create")
					return
				}
				write(`{"type":"response.created","response":{"id":"r1"}}`)
				write(`{"type":"response.output_item.added","output_index":0,"item":` + question + `}`)
				write(`{"type":"response.output_item.done","output_index":0,"item":` + question + `}`)
				if scenario == "completed_answer" {
					write(`{"type":"response.completed","response":{"id":"r1","status":"completed","output":[` + question + `]}}`)
				} else {
					want := interrupt
					if scenario == "active_steer" {
						want = steer
					}
					if got := read(); !bytes.Equal(got, want) {
						t.Errorf("active answer control changed: %s", got)
						return
					}
					if scenario == "active_steer" {
						write(`{"type":"response.steer.accepted","steer":{"id":"s1","previous_response_id":"r1"}}`)
					}
					write(`{"type":"response.incomplete","response":{"id":"r1","status":"incomplete","incomplete_details":{"reason":"interrupted"},"output":[` + question + `]}}`)
				}
				if scenario != "active_steer" {
					got := read()
					if gjson.GetBytes(got, "type").String() != "response.create" || gjson.GetBytes(got, "previous_response_id").String() != "r1" || gjson.GetBytes(got, "input").Raw != answerInput {
						t.Errorf("question answer or conversation identity lost: %s", got)
						return
					}
				}
				write(`{"type":"response.created","response":{"id":"r2"}}`)
				write(`{"type":"response.completed","response":{"id":"r2","status":"completed","output":[{"id":"msg_answer","type":"message","role":"assistant","content":[{"type":"output_text","text":"Green acknowledged"}]}]}}`)
				_, _, _ = c.ReadMessage()
			}))
			defer upstream.Close()
			cfg := &config.Config{}
			cfg.Codex.ResponseSteering = scenario == "active_steer" || scenario == "active_interrupt_duplex"
			cfg.CodexResponseSteering = cfg.Codex.ResponseSteering
			manager := coreauth.NewManager(nil, nil, nil)
			manager.SetConfig(cfg)
			manager.RegisterExecutor(runtimeexecutor.NewCodexAutoExecutor(cfg))
			authID := "async-question-" + scenario
			model := "async-question-model-" + scenario
			if _, err := manager.Register(context.Background(), &coreauth.Auth{ID: authID, Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"api_key": "test-key", "base_url": upstream.URL, "websockets": "true"}}); err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model}})
			defer registry.GetGlobalRegistry().UnregisterClient(authID)
			h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager))
			router := gin.New()
			router.GET("/v1/responses", h.ResponsesWebsocket)
			downstream := httptest.NewServer(router)
			defer downstream.Close()
			c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(downstream.URL, "http")+"/v1/responses", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
			send := func(payload []byte) {
				t.Helper()
				if err := c.WriteMessage(websocket.TextMessage, payload); err != nil {
					t.Fatal(err)
				}
			}
			send([]byte(fmt.Sprintf(`{"type":"response.create","model":%q,"input":[]}`, model)))
			added, questionSeen, terminalQuestion, accepted, completed := false, false, false, false, false
			for !completed {
				_, payload, err := c.ReadMessage()
				if err != nil {
					t.Fatalf("early downstream close: %v", err)
				}
				kind := gjson.GetBytes(payload, "type").String()
				switch kind {
				case "response.output_item.added", "response.output_item.done":
					if got := gjson.GetBytes(payload, "item").Raw; got != question {
						t.Fatalf("async question metadata changed: %s", payload)
					}
					if kind == "response.output_item.added" {
						added = true
					} else {
						questionSeen = true
						if scenario == "active_steer" {
							send(steer)
						} else if scenario != "completed_answer" {
							send(interrupt)
						}
					}
				case "response.steer.accepted":
					accepted = true
				case "response.incomplete", "response.completed":
					if gjson.GetBytes(payload, "response.id").String() == "r1" {
						if gjson.GetBytes(payload, "response.output.0").Raw != question {
							t.Fatalf("terminal question metadata changed: %s", payload)
						}
						terminalQuestion = true
						if scenario != "active_steer" {
							send([]byte(fmt.Sprintf(`{"type":"response.create","model":%q,"previous_response_id":"r1","input":%s}`, model, answerInput)))
						}
					} else if gjson.GetBytes(payload, "response.id").String() == "r2" {
						completed = kind == "response.completed" && gjson.GetBytes(payload, "response.output.0.content.0.text").String() == "Green acknowledged"
						if !completed {
							t.Fatalf("answer successor failed: %s", payload)
						}
					}
				case "error", "response.failed", "response.steer.rejected":
					t.Fatalf("async answer failed: %s", payload)
				}
			}
			if !added || !questionSeen || !terminalQuestion || scenario == "active_steer" && !accepted {
				t.Fatal("question or steering acknowledgement was lost")
			}
			_ = c.Close()
			select {
			case <-upstreamDone:
			case <-time.After(3 * time.Second):
				t.Fatal("upstream cleanup stalled")
			}
			if connections.Load() != 1 {
				t.Fatalf("unexpected reconnect/replay: %d", connections.Load())
			}
		})
	}
}
